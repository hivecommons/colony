// Package devstats is the colony provider for CNCF DevStats contribution
// activity (https://devstats.cncf.io).
//
// It sends the person's GitHub login, and nothing else, to the public
// DevStats API. A query that carries only email addresses returns an empty
// result without making any request.
package devstats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/hivecommons/colony/fetch"
	"github.com/hivecommons/colony/resolver"
)

// SourceName is stamped on every fact this provider returns.
const SourceName = "devstats"

const (
	// DefaultBaseURL is the DevStats JSON API endpoint.
	DefaultBaseURL = "https://devstats.cncf.io/api/v1"
	// DefaultDashboardURL is the All CNCF "Developer Activity Counts by
	// Repository Group Table" dashboard that facts link to.
	DefaultDashboardURL = "https://all.devstats.cncf.io/d/9/developer-activity-counts-by-repository-group-table"
	// DefaultRange is the DevStats range used for the per-project breakdown.
	DefaultRange = "Last year"
	// DefaultRateLimit is the sustained request rate to DevStats.
	DefaultRateLimit = rate.Limit(1)
	// DefaultBurst is the rate limiter burst.
	DefaultBurst = 2
	// DefaultCacheTTL is how long a person's answer is reused.
	DefaultCacheTTL = 24 * time.Hour
	// DefaultMaxCacheEntries bounds the in-memory cache.
	DefaultMaxCacheEntries = 1024

	// AllCNCF is the Project label of the CNCF-wide all-time total.
	AllCNCF = "All CNCF"
	// AllTime is the Period label of the CNCF-wide total.
	AllTime = "all time"
	// totalDashboardRange is the widest dashboard range, linked from the
	// all-time total.
	totalDashboardRange = "Last decade"

	apiGithubIDContributions = "GithubIDContributions"
	apiDevActCnt             = "DevActCnt"
	apiRepoGroups            = "RepoGroups"
	allProjectsDB            = "all"
	metricContributions      = "Contributions"
	allCountries             = "All"
	allRepoGroups            = "All"
	notFoundMarker           = "not found in results"
)

// githubLogin is GitHub's login syntax: alphanumerics and single inner
// hyphens, at most 39 characters. Anything else is refused before any
// request is made.
var githubLogin = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9]|-[A-Za-z0-9]){0,38}$`)

// ErrInvalidLogin is returned for a GitHub login that cannot be valid.
var ErrInvalidLogin = errors.New("devstats: invalid GitHub login")

// APIError is an error message returned by the DevStats API.
type APIError struct {
	API     string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("devstats: %s: %s", e.API, e.Message)
}

// Options configures a Provider. The zero value asks DevStats for the
// CNCF-wide total only.
type Options struct {
	// BaseURL is the API endpoint; DefaultBaseURL when empty.
	BaseURL string
	// DashboardURL is the dashboard facts link to; DefaultDashboardURL when
	// empty.
	DashboardURL string
	// Projects lists the DevStats repository groups of the All CNCF
	// database (CNCF project names such as "Kubernetes" or "Hive Commons")
	// to break activity down by. Each costs one request per person. See
	// Provider.Projects for the valid names.
	Projects []string
	// Range is the DevStats range for the per-project breakdown, such as
	// "Last year" or "Last decade"; DefaultRange when empty.
	Range string
	// CacheTTL is how long an answer is reused; DefaultCacheTTL when zero.
	CacheTTL time.Duration
	// MaxCacheEntries bounds the cache; DefaultMaxCacheEntries when zero.
	MaxCacheEntries int
	// Client overrides the HTTP client. The default is rate-limited to
	// DefaultRateLimit.
	Client *fetch.Client
	// Now overrides the clock (tests).
	Now func() time.Time
}

type cacheEntry struct {
	res     resolver.Result
	fetched time.Time
}

// Provider is the DevStats resolver.Provider. It is safe for concurrent use.
type Provider struct {
	opts   Options
	client *fetch.Client

	mu    sync.Mutex
	cache map[string]cacheEntry
}

// New returns a DevStats Provider.
func New(opts Options) *Provider {
	if opts.BaseURL == "" {
		opts.BaseURL = DefaultBaseURL
	}
	if opts.DashboardURL == "" {
		opts.DashboardURL = DefaultDashboardURL
	}
	if opts.Range == "" {
		opts.Range = DefaultRange
	}
	if opts.CacheTTL <= 0 {
		opts.CacheTTL = DefaultCacheTTL
	}
	if opts.MaxCacheEntries <= 0 {
		opts.MaxCacheEntries = DefaultMaxCacheEntries
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	client := opts.Client
	if client == nil {
		client = fetch.New(fetch.Options{RateLimit: DefaultRateLimit, Burst: DefaultBurst})
	}
	return &Provider{opts: opts, client: client, cache: map[string]cacheEntry{}}
}

// Name implements resolver.Provider.
func (p *Provider) Name() string { return SourceName }

// Lookup implements resolver.Provider. Only q.GitHubLogin is used; emails
// are never sent.
func (p *Provider) Lookup(ctx context.Context, q resolver.Query) (resolver.Result, error) {
	if q.Empty() {
		return resolver.Result{}, resolver.ErrEmptyQuery
	}
	login := strings.TrimPrefix(strings.TrimSpace(q.GitHubLogin), "@")
	if login == "" {
		return resolver.Result{}, nil
	}
	if !githubLogin.MatchString(login) {
		return resolver.Result{}, ErrInvalidLogin
	}
	key := strings.ToLower(login)
	if res, ok := p.cached(key); ok {
		return res, nil
	}
	res, err := p.fetch(ctx, login)
	if err != nil {
		return resolver.Result{}, err
	}
	p.store(key, res)
	return res, nil
}

func (p *Provider) cached(key string) (resolver.Result, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.cache[key]
	if !ok || p.opts.Now().Sub(e.fetched) >= p.opts.CacheTTL {
		return resolver.Result{}, false
	}
	return e.res, true
}

func (p *Provider) store(key string, res resolver.Result) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.opts.Now()
	if _, exists := p.cache[key]; !exists && len(p.cache) >= p.opts.MaxCacheEntries {
		p.evictLocked(now)
	}
	p.cache[key] = cacheEntry{res: res, fetched: now}
}

// evictLocked drops expired entries, and the oldest one if none expired.
func (p *Provider) evictLocked(now time.Time) {
	var oldestKey string
	var oldest time.Time
	for k, e := range p.cache {
		if now.Sub(e.fetched) >= p.opts.CacheTTL {
			delete(p.cache, k)
			continue
		}
		if oldestKey == "" || e.fetched.Before(oldest) {
			oldestKey, oldest = k, e.fetched
		}
	}
	if len(p.cache) >= p.opts.MaxCacheEntries && oldestKey != "" {
		delete(p.cache, oldestKey)
	}
}

type request struct {
	API     string `json:"api"`
	Payload any    `json:"payload,omitempty"`
}

type idContributionsPayload struct {
	GithubID string `json:"github_id"`
}

type idContributionsResponse struct {
	Contributions int    `json:"contributions"`
	Issues        int    `json:"issues"`
	PRs           int    `json:"prs"`
	Error         string `json:"error"`
}

type devActCntPayload struct {
	Project         string `json:"project"`
	Range           string `json:"range"`
	Metric          string `json:"metric"`
	RepositoryGroup string `json:"repository_group"`
	Country         string `json:"country"`
	GithubID        string `json:"github_id"`
}

type devActCntResponse struct {
	Login  []string `json:"login"`
	Number []int    `json:"number"`
	Rank   []int    `json:"rank"`
	Error  string   `json:"error"`
}

func (p *Provider) call(ctx context.Context, api string, payload, out any) error {
	err := p.client.PostJSON(ctx, p.opts.BaseURL, nil, request{API: api, Payload: payload}, out)
	var se *fetch.StatusError
	if errors.As(err, &se) {
		var body struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(se.Body, &body) == nil && body.Error != "" {
			return &APIError{API: api, Message: body.Error}
		}
	}
	return err
}

func (p *Provider) fetch(ctx context.Context, login string) (resolver.Result, error) {
	var res resolver.Result
	var total idContributionsResponse
	if err := p.call(ctx, apiGithubIDContributions, idContributionsPayload{GithubID: login}, &total); err != nil {
		return res, err
	}
	if total.Error != "" {
		return res, &APIError{API: apiGithubIDContributions, Message: total.Error}
	}
	if total.Contributions > 0 {
		res.Activity = append(res.Activity, resolver.Activity{
			Project:       AllCNCF,
			Contributions: total.Contributions,
			Period:        AllTime,
			URL:           p.dashboardLink(allRepoGroups, totalDashboardRange),
			Source:        SourceName,
		})
	}
	for _, project := range p.opts.Projects {
		n, err := p.projectContributions(ctx, project, login)
		if err != nil {
			return resolver.Result{}, err
		}
		if n > 0 {
			res.Activity = append(res.Activity, resolver.Activity{
				Project:       project,
				Contributions: n,
				Period:        p.opts.Range,
				URL:           p.dashboardLink(project, p.opts.Range),
				Source:        SourceName,
			})
		}
	}
	if len(res.Activity) > 0 {
		res.Sources = []string{SourceName}
	}
	return res, nil
}

func (p *Provider) projectContributions(ctx context.Context, project, login string) (int, error) {
	var out devActCntResponse
	err := p.call(ctx, apiDevActCnt, devActCntPayload{
		Project:         allProjectsDB,
		Range:           p.opts.Range,
		Metric:          metricContributions,
		RepositoryGroup: project,
		Country:         allCountries,
		GithubID:        login,
	}, &out)
	var apiErr *APIError
	if errors.As(err, &apiErr) && strings.Contains(apiErr.Message, notFoundMarker) {
		return 0, nil // DevStats answers 400 for a person with no activity
	}
	if err != nil {
		return 0, err
	}
	if out.Error != "" {
		return 0, &APIError{API: apiDevActCnt, Message: out.Error}
	}
	total := 0
	for i, l := range out.Login {
		if strings.EqualFold(l, login) && i < len(out.Number) {
			total += out.Number[i]
		}
	}
	return total, nil
}

// dashboardLink is a project-level dashboard view; it carries no
// per-person data.
func (p *Provider) dashboardLink(repoGroup, period string) string {
	v := url.Values{}
	v.Set("orgId", "1")
	v.Set("var-period_name", period)
	v.Set("var-metric", strings.ToLower(metricContributions))
	v.Set("var-repogroup_name", repoGroup)
	v.Set("var-country_name", allCountries)
	return p.opts.DashboardURL + "?" + v.Encode()
}

type repoGroupsPayload struct {
	Project string `json:"project"`
}

type repoGroupsResponse struct {
	RepoGroups []string `json:"repo_groups"`
	Error      string   `json:"error"`
}

// Projects lists the repository groups of the All CNCF database: the valid
// values for Options.Projects. It sends no per-person data.
func (p *Provider) Projects(ctx context.Context) ([]string, error) {
	var out repoGroupsResponse
	if err := p.call(ctx, apiRepoGroups, repoGroupsPayload{Project: allProjectsDB}, &out); err != nil {
		return nil, err
	}
	if out.Error != "" {
		return nil, &APIError{API: apiRepoGroups, Message: out.Error}
	}
	groups := make([]string, 0, len(out.RepoGroups))
	for _, g := range out.RepoGroups {
		if g != allRepoGroups {
			groups = append(groups, g)
		}
	}
	return groups, nil
}
