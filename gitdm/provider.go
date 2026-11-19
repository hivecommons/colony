package gitdm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/colony/fetch"
	"github.com/hivecommons/colony/resolver"
)

const (
	// DefaultBaseURL is the raw-content root of cncf/gitdm's default branch.
	DefaultBaseURL = "https://raw.githubusercontent.com/cncf/gitdm/master/"
	// DefaultRefresh is how long a loaded snapshot is used before the files
	// are revalidated (a cheap 304 when nothing changed).
	DefaultRefresh = 24 * time.Hour
	// DefaultRetryAfterFailure is how long a failed revalidation waits
	// before the next attempt while the old snapshot keeps serving.
	DefaultRetryAfterFailure = 15 * time.Minute
	// MaxAffiliationFiles bounds discovery of developers_affiliationsN.txt.
	MaxAffiliationFiles = 64
	// AliasesFile is gitdm's email alias file.
	AliasesFile = "src/cncf-config/aliases"

	affiliationFilePattern = "developers_affiliations%d.txt"
)

// AffiliationFile returns the name of the n-th (1-based) affiliation file.
func AffiliationFile(n int) string {
	return fmt.Sprintf(affiliationFilePattern, n)
}

// Options configures a Provider. The zero value downloads from cncf/gitdm
// without a disk cache and refreshes daily.
type Options struct {
	// BaseURL is the root the file names are resolved against.
	BaseURL string
	// AffiliationFiles lists the developers_affiliations files to load. Nil
	// discovers developers_affiliations1.txt, 2, ... until the first 404.
	AffiliationFiles []string
	// AliasFiles lists email alias files. Nil means []string{AliasesFile};
	// an empty non-nil slice disables aliases.
	AliasFiles []string
	// Refresh is the snapshot lifetime; DefaultRefresh when zero.
	Refresh time.Duration
	// RetryAfterFailure spaces out revalidation attempts after a failure;
	// DefaultRetryAfterFailure when zero.
	RetryAfterFailure time.Duration
	// CacheDir enables the ETag disk cache when Client is nil.
	CacheDir string
	// Client overrides the HTTP client. When set, CacheDir is ignored and
	// the client's own cache settings apply.
	Client *fetch.Client
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Provider is the gitdm resolver.Provider. It is safe for concurrent use.
type Provider struct {
	opts   Options
	client *fetch.Client

	mu       sync.Mutex
	idx      *Index
	loadedAt time.Time
	retryAt  time.Time
}

// New returns a gitdm Provider. Nothing is downloaded until the first
// Lookup or Refresh.
func New(opts Options) *Provider {
	if opts.BaseURL == "" {
		opts.BaseURL = DefaultBaseURL
	}
	if !strings.HasSuffix(opts.BaseURL, "/") {
		opts.BaseURL += "/"
	}
	if opts.AliasFiles == nil {
		opts.AliasFiles = []string{AliasesFile}
	}
	if opts.Refresh <= 0 {
		opts.Refresh = DefaultRefresh
	}
	if opts.RetryAfterFailure <= 0 {
		opts.RetryAfterFailure = DefaultRetryAfterFailure
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	client := opts.Client
	if client == nil {
		client = fetch.New(fetch.Options{CacheDir: opts.CacheDir})
	}
	return &Provider{opts: opts, client: client}
}

// Name implements resolver.Provider.
func (p *Provider) Name() string { return SourceName }

// LoadedAt is when the current snapshot was loaded; zero before the first
// successful load.
func (p *Provider) LoadedAt() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.loadedAt
}

// Refresh reloads the snapshot now, regardless of its age. On failure the
// previous snapshot, if any, stays in use.
func (p *Provider) Refresh(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reloadLocked(ctx)
}

// Lookup implements resolver.Provider. It only reads the local snapshot,
// loading or revalidating it first when it is missing or older than
// Options.Refresh. A failed revalidation keeps serving the old snapshot.
func (p *Provider) Lookup(ctx context.Context, q resolver.Query) (resolver.Result, error) {
	if q.Empty() {
		return resolver.Result{}, resolver.ErrEmptyQuery
	}
	idx, err := p.index(ctx)
	if err != nil {
		return resolver.Result{}, err
	}
	return idx.Lookup(q), nil
}

func (p *Provider) index(ctx context.Context) (*Index, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.opts.Now()
	if p.idx != nil && (now.Sub(p.loadedAt) < p.opts.Refresh || now.Before(p.retryAt)) {
		return p.idx, nil
	}
	if err := p.reloadLocked(ctx); err != nil {
		if p.idx == nil {
			return nil, err
		}
		p.retryAt = now.Add(p.opts.RetryAfterFailure)
	}
	return p.idx, nil
}

func (p *Provider) reloadLocked(ctx context.Context) error {
	devs, err := p.loadAffiliations(ctx)
	if err != nil {
		return err
	}
	var aliases [][2]string
	for _, name := range p.opts.AliasFiles {
		resp, err := p.client.GetCached(ctx, p.opts.BaseURL+name)
		if err != nil {
			return fmt.Errorf("gitdm: %s: %w", name, err)
		}
		pairs, err := ParseAliases(bytes.NewReader(resp.Body))
		if err != nil {
			return err
		}
		aliases = append(aliases, pairs...)
	}
	p.idx = NewIndex(devs, aliases)
	p.loadedAt = p.opts.Now()
	return nil
}

func (p *Provider) loadAffiliations(ctx context.Context) ([]Developer, error) {
	files := p.opts.AffiliationFiles
	discover := files == nil
	if discover {
		files = make([]string, 0, MaxAffiliationFiles)
		for n := 1; n <= MaxAffiliationFiles; n++ {
			files = append(files, AffiliationFile(n))
		}
	}
	var devs []Developer
	loaded := 0
	for _, name := range files {
		resp, err := p.client.GetCached(ctx, p.opts.BaseURL+name)
		if err != nil {
			if discover && loaded > 0 && errors.Is(err, fetch.ErrNotFound) {
				break
			}
			return nil, fmt.Errorf("gitdm: %s: %w", name, err)
		}
		parsed, err := ParseAffiliations(bytes.NewReader(resp.Body))
		if err != nil {
			return nil, fmt.Errorf("gitdm: %s: %w", name, err)
		}
		devs = append(devs, parsed...)
		loaded++
	}
	return devs, nil
}
