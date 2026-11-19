package gitdm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/colony/fetch"
	"github.com/hivecommons/colony/resolver"
)

// fixtureServer serves testdata at the paths cncf/gitdm uses and records
// every request it sees.
type fixtureServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []*http.Request
	bodies   []string
	fail     atomic.Bool
}

func newFixtureServer(t *testing.T) *fixtureServer {
	t.Helper()
	files := map[string]string{
		"/" + AffiliationFile(1): "testdata/developers_affiliations1.txt",
		"/" + AffiliationFile(2): "testdata/developers_affiliations2.txt",
		"/" + AliasesFile:        "testdata/aliases",
	}
	fs := &fixtureServer{}
	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fs.mu.Lock()
		fs.requests = append(fs.requests, r.Clone(context.Background()))
		fs.bodies = append(fs.bodies, string(body))
		fs.mu.Unlock()
		if fs.fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		src, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		data, err := os.ReadFile(src)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		etag := `"` + path.Base(src) + `"`
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		_, _ = w.Write(data)
	}))
	t.Cleanup(fs.Close)
	return fs
}

func (fs *fixtureServer) count() int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return len(fs.requests)
}

func TestLookupByLogin(t *testing.T) {
	srv := newFixtureServer(t)
	p := New(Options{BaseURL: srv.URL})
	res, err := p.Lookup(context.Background(), resolver.Query{GitHubLogin: "DIMS"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Affiliations) != 7 {
		t.Fatalf("affiliations = %+v", res.Affiliations)
	}
	cur := res.CurrentAffiliations(day("2026-11-19"))
	if len(cur) != 1 || cur[0].Org != "NVIDIA Corporation" {
		t.Errorf("current = %+v", cur)
	}
	for _, a := range res.Affiliations {
		if a.Source != SourceName || a.Confidence != resolver.ConfidenceHigh || a.MatchedBy != matchedByLogin {
			t.Errorf("fact provenance = %+v", a)
		}
	}
	if len(res.Sources) != 1 || res.Sources[0] != SourceName {
		t.Errorf("sources = %v", res.Sources)
	}
}

func TestLookupByEmailAndCaseVariants(t *testing.T) {
	srv := newFixtureServer(t)
	p := New(Options{BaseURL: srv.URL})
	res, err := p.Lookup(context.Background(), resolver.Query{Emails: []string{" Jordan@Liggitt.net "}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Affiliations) != 2 || res.Affiliations[1].Org != "Google LLC" ||
		res.Affiliations[1].Confidence != resolver.ConfidenceMedium || res.Affiliations[1].MatchedBy != matchedByEmail {
		t.Errorf("email match = %+v", res.Affiliations)
	}
	// AdoHe (file 1) and adohe (file 2) are the same GitHub account; both
	// say Alibaba Group, which must be reported once.
	res, err = p.Lookup(context.Background(), resolver.Query{GitHubLogin: "adohe"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Affiliations) != 1 || res.Affiliations[0].Org != "Alibaba Group" {
		t.Errorf("case variants = %+v", res.Affiliations)
	}
	// Login and email naming the same entry: reported once, as the login match.
	res, err = p.Lookup(context.Background(), resolver.Query{GitHubLogin: "thockin", Emails: []string{"thockin@google.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Affiliations) != 1 || res.Affiliations[0].MatchedBy != matchedByLogin {
		t.Errorf("login+email = %+v", res.Affiliations)
	}
	res, err = p.Lookup(context.Background(), resolver.Query{GitHubLogin: "nobody-here"})
	if err != nil || !res.Empty() || res.Sources != nil {
		t.Errorf("unknown = %+v, %v", res, err)
	}
	if _, err := p.Lookup(context.Background(), resolver.Query{}); !errors.Is(err, resolver.ErrEmptyQuery) {
		t.Errorf("empty query = %v", err)
	}
}

func TestAliasExpansion(t *testing.T) {
	devs := []Developer{{Login: "a", Emails: []string{"canon@x.org"}, Affiliations: []Period{{Org: "Acme"}}}}
	aliases := [][2]string{{"old@x.org", "mid@x.org"}, {"mid@x.org", "canon@x.org"}}
	ix := NewIndex(devs, aliases)
	if ix.Developers() != 1 {
		t.Fatal("Developers()")
	}
	res := ix.Lookup(resolver.Query{Emails: []string{"OLD@x.org"}})
	if len(res.Affiliations) != 1 || res.Affiliations[0].Org != "Acme" {
		t.Errorf("transitive alias = %+v", res.Affiliations)
	}
}

// TestLookupOnlyFetchesSnapshotURLs is the privacy guarantee of this
// provider: a lookup downloads the same public snapshot files for everyone,
// with GET and no query string, and the login or email being looked up
// never appears anywhere in any request.
func TestLookupOnlyFetchesSnapshotURLs(t *testing.T) {
	srv := newFixtureServer(t)
	p := New(Options{BaseURL: srv.URL, CacheDir: t.TempDir()})
	login, email := "liggitt", "jordan@liggitt.net"
	if _, err := p.Lookup(context.Background(), resolver.Query{GitHubLogin: login, Emails: []string{email}}); err != nil {
		t.Fatal(err)
	}
	if err := p.Refresh(context.Background()); err != nil { // revalidation path too
		t.Fatal(err)
	}
	allowed := map[string]bool{
		"/" + AffiliationFile(1): true,
		"/" + AffiliationFile(2): true,
		"/" + AffiliationFile(3): true, // the discovery probe that ends the list
		"/" + AliasesFile:        true,
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.requests) == 0 {
		t.Fatal("no requests recorded")
	}
	needles := []string{login, email, "liggitt.net", "jordan"}
	for i, r := range srv.requests {
		if r.Method != http.MethodGet {
			t.Errorf("request %d: method %s, want GET", i, r.Method)
		}
		if !allowed[r.URL.Path] || r.URL.RawQuery != "" {
			t.Errorf("request %d: unexpected URL %s", i, r.URL.String())
		}
		if srv.bodies[i] != "" {
			t.Errorf("request %d: unexpected body %q", i, srv.bodies[i])
		}
		if ua := r.Header.Get("User-Agent"); ua != fetch.UserAgent() {
			t.Errorf("request %d: User-Agent %q", i, ua)
		}
		var hdr strings.Builder
		for k, vs := range r.Header {
			hdr.WriteString(k + ": " + strings.Join(vs, ",") + "\n")
		}
		for _, n := range needles {
			if strings.Contains(strings.ToLower(r.URL.String()+hdr.String()), n) {
				t.Errorf("request %d carries %q", i, n)
			}
		}
	}
}

func TestRefreshAndStaleSnapshot(t *testing.T) {
	srv := newFixtureServer(t)
	now := day("2026-01-01")
	clock := func() time.Time { return now }
	p := New(Options{BaseURL: srv.URL, Now: clock, Refresh: time.Hour, RetryAfterFailure: time.Minute})
	q := resolver.Query{GitHubLogin: "thockin"}
	if _, err := p.Lookup(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	if !p.LoadedAt().Equal(now) {
		t.Errorf("LoadedAt = %v", p.LoadedAt())
	}
	loads := srv.count()
	if _, err := p.Lookup(context.Background(), q); err != nil || srv.count() != loads {
		t.Fatalf("fresh snapshot must not refetch (requests %d -> %d, err %v)", loads, srv.count(), err)
	}
	// Past the refresh interval with the server down: old snapshot serves.
	now = now.Add(2 * time.Hour)
	srv.fail.Store(true)
	res, err := p.Lookup(context.Background(), q)
	if err != nil || len(res.Affiliations) != 1 {
		t.Fatalf("stale snapshot must keep serving: %+v, %v", res, err)
	}
	failed := srv.count()
	if _, err := p.Lookup(context.Background(), q); err != nil || srv.count() != failed {
		t.Fatalf("a failed refresh must back off (requests %d -> %d)", failed, srv.count())
	}
	if err := p.Refresh(context.Background()); err == nil {
		t.Error("explicit Refresh must report the failure")
	}
	// After the backoff, with the server back, the snapshot reloads.
	now = now.Add(2 * time.Minute)
	srv.fail.Store(false)
	if _, err := p.Lookup(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	if !p.LoadedAt().Equal(now) {
		t.Errorf("snapshot not reloaded: LoadedAt = %v", p.LoadedAt())
	}
}

func TestFirstLoadFailure(t *testing.T) {
	srv := newFixtureServer(t)
	srv.fail.Store(true)
	p := New(Options{BaseURL: srv.URL})
	if _, err := p.Lookup(context.Background(), resolver.Query{GitHubLogin: "x"}); err == nil {
		t.Fatal("no snapshot and no server must fail")
	}
	if p.Name() != SourceName {
		t.Errorf("Name = %q", p.Name())
	}
}

func TestExplicitFilesAndMissingAliasFile(t *testing.T) {
	srv := newFixtureServer(t)
	p := New(Options{BaseURL: srv.URL + "/", AffiliationFiles: []string{AffiliationFile(2)}, AliasFiles: []string{}})
	res, err := p.Lookup(context.Background(), resolver.Query{GitHubLogin: "brendandburns"})
	if err != nil || len(res.Affiliations) != 2 {
		t.Fatalf("explicit file = %+v, %v", res, err)
	}
	if res, _ := p.Lookup(context.Background(), resolver.Query{GitHubLogin: "dims"}); !res.Empty() {
		t.Error("file 1 was not configured and must not be loaded")
	}
	// A listed file that is missing is an error, not the end of discovery.
	bad := New(Options{BaseURL: srv.URL, AffiliationFiles: []string{AffiliationFile(1), "nope.txt"}})
	if err := bad.Refresh(context.Background()); !errors.Is(err, fetch.ErrNotFound) {
		t.Errorf("missing listed file = %v", err)
	}
	noAliases := New(Options{BaseURL: srv.URL, AliasFiles: []string{"nope"}})
	if err := noAliases.Refresh(context.Background()); !errors.Is(err, fetch.ErrNotFound) {
		t.Errorf("missing alias file = %v", err)
	}
	// Discovery with no first file is an error.
	empty := New(Options{BaseURL: srv.URL + "/nothing/"})
	if err := empty.Refresh(context.Background()); !errors.Is(err, fetch.ErrNotFound) {
		t.Errorf("no files = %v", err)
	}
}
