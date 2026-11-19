package devstats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/colony/fetch"
	"github.com/hivecommons/colony/resolver"
)

// fakeDevStats mimics the DevStats API: JSON POSTs to one endpoint, errors
// as HTTP 400 with {"error": "..."}.
type fakeDevStats struct {
	*httptest.Server
	mu       sync.Mutex
	requests []map[string]any
	raw      []string
	totals   map[string]int
	projects map[string]map[string]int // repository group -> login -> count
	broken   bool
}

func newFake(t *testing.T) *fakeDevStats {
	t.Helper()
	f := &fakeDevStats{
		totals: map[string]int{"dims": 185345},
		projects: map[string]map[string]int{
			"Kubernetes":   {"dims": 2388},
			"Hive Commons": {},
		},
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeDevStats) fail(w http.ResponseWriter, msg string) {
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (f *fakeDevStats) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		API     string            `json:"api"`
		Payload map[string]string `json:"payload"`
	}
	var generic map[string]any
	_ = json.Unmarshal(body, &generic)
	f.mu.Lock()
	f.requests = append(f.requests, generic)
	f.raw = append(f.raw, r.Method+" "+r.URL.String()+" "+string(body))
	broken := f.broken
	f.mu.Unlock()
	if broken {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost || json.Unmarshal(body, &req) != nil {
		f.fail(w, "bad request")
		return
	}
	id := req.Payload["github_id"]
	switch req.API {
	case apiGithubIDContributions:
		n := f.totals[id]
		_ = json.NewEncoder(w).Encode(map[string]int{"contributions": n, "issues": n / 10, "prs": n / 5})
	case apiDevActCnt:
		group, ok := f.projects[req.Payload["repository_group"]]
		if !ok {
			f.fail(w, fmt.Sprintf("API 'DevActCnt': invalid repository_group name: '%s'", req.Payload["repository_group"]))
			return
		}
		n, ok := group[id]
		if !ok {
			f.fail(w, fmt.Sprintf("API 'DevActCnt': github_id '%s' not found in results", id))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"rank": []int{15}, "login": []string{id}, "number": []int{n}})
	case apiRepoGroups:
		_ = json.NewEncoder(w).Encode(map[string]any{"repo_groups": []string{"Agones", "All", "Kubernetes"}})
	default:
		f.fail(w, "unknown API")
	}
}

func (f *fakeDevStats) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func newProvider(f *fakeDevStats, opts Options) *Provider {
	opts.BaseURL = f.URL
	if opts.Client == nil {
		opts.Client = fetch.New(fetch.Options{})
	}
	return New(opts)
}

func TestLookupTotalsAndProjects(t *testing.T) {
	f := newFake(t)
	p := newProvider(f, Options{Projects: []string{"Kubernetes", "Hive Commons"}})
	res, err := p.Lookup(context.Background(), resolver.Query{GitHubLogin: "@dims"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Activity) != 2 {
		t.Fatalf("activity = %+v", res.Activity)
	}
	total, k8s := res.Activity[0], res.Activity[1]
	if total.Project != AllCNCF || total.Contributions != 185345 || total.Period != AllTime || total.Source != SourceName {
		t.Errorf("total = %+v", total)
	}
	if k8s.Project != "Kubernetes" || k8s.Contributions != 2388 || k8s.Period != DefaultRange {
		t.Errorf("kubernetes = %+v", k8s)
	}
	u, err := url.Parse(k8s.URL)
	if err != nil || !strings.HasPrefix(k8s.URL, DefaultDashboardURL) ||
		u.Query().Get("var-repogroup_name") != "Kubernetes" || u.Query().Get("var-period_name") != DefaultRange {
		t.Errorf("dashboard link = %q", k8s.URL)
	}
	if strings.Contains(strings.ToLower(k8s.URL), "dims") {
		t.Error("the dashboard link must not carry the login")
	}
	if len(res.Sources) != 1 || res.Sources[0] != SourceName {
		t.Errorf("sources = %v", res.Sources)
	}
	// Request shape matches the DevStats API.
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 3 {
		t.Fatalf("requests = %v", f.raw)
	}
	dev := f.requests[1]
	payload, _ := dev["payload"].(map[string]any)
	if dev["api"] != apiDevActCnt || payload["project"] != "all" || payload["metric"] != "Contributions" ||
		payload["country"] != "All" || payload["range"] != DefaultRange || payload["github_id"] != "dims" {
		t.Errorf("DevActCnt request = %v", dev)
	}
}

func TestUnknownLoginIsEmptyNotError(t *testing.T) {
	f := newFake(t)
	p := newProvider(f, Options{Projects: []string{"Kubernetes"}})
	res, err := p.Lookup(context.Background(), resolver.Query{GitHubLogin: "nobody-here"})
	if err != nil {
		t.Fatalf("a login without activity is not an error: %v", err)
	}
	if !res.Empty() || res.Sources != nil {
		t.Errorf("res = %+v", res)
	}
}

// TestNeverSendsEmail is this provider's privacy guarantee: only the
// GitHub login is ever sent, and an email-only query makes no request.
func TestNeverSendsEmail(t *testing.T) {
	f := newFake(t)
	p := newProvider(f, Options{Projects: []string{"Kubernetes"}})
	email := "davanum@gmail.com"
	res, err := p.Lookup(context.Background(), resolver.Query{Emails: []string{email}})
	if err != nil || !res.Empty() {
		t.Fatalf("email-only = %+v, %v", res, err)
	}
	if f.count() != 0 {
		t.Fatalf("an email-only query must make no request, got %d", f.count())
	}
	if _, err := p.Lookup(context.Background(), resolver.Query{GitHubLogin: "dims", Emails: []string{email}}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.raw {
		if strings.Contains(r, email) || strings.Contains(r, "gmail") || strings.Contains(r, "@") {
			t.Errorf("request carries an email: %s", r)
		}
	}
}

func TestInvalidLoginAndEmptyQuery(t *testing.T) {
	f := newFake(t)
	p := newProvider(f, Options{})
	for _, bad := range []string{"has space", "-lead", "trail-", "a--b", "x/../y", strings.Repeat("a", 40)} {
		if _, err := p.Lookup(context.Background(), resolver.Query{GitHubLogin: bad}); !errors.Is(err, ErrInvalidLogin) {
			t.Errorf("%q: want ErrInvalidLogin, got %v", bad, err)
		}
	}
	if _, err := p.Lookup(context.Background(), resolver.Query{}); !errors.Is(err, resolver.ErrEmptyQuery) {
		t.Errorf("empty query = %v", err)
	}
	if f.count() != 0 {
		t.Errorf("invalid input must not reach the API, got %d requests", f.count())
	}
	if p.Name() != SourceName {
		t.Errorf("Name = %q", p.Name())
	}
}

func TestCacheTTLAndBound(t *testing.T) {
	f := newFake(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := newProvider(f, Options{CacheTTL: time.Hour, MaxCacheEntries: 2, Now: func() time.Time { return now }})
	q := resolver.Query{GitHubLogin: "dims"}
	if _, err := p.Lookup(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Lookup(context.Background(), resolver.Query{GitHubLogin: "DIMS"}); err != nil || f.count() != 1 {
		t.Fatalf("cached (case-insensitive) lookup must not call the API: %d requests", f.count())
	}
	now = now.Add(2 * time.Hour)
	if _, err := p.Lookup(context.Background(), q); err != nil || f.count() != 2 {
		t.Fatalf("expired entry must refetch: %d requests", f.count())
	}
	// Fill past the bound: the oldest entry is evicted.
	for _, l := range []string{"a1", "a2"} {
		now = now.Add(time.Minute)
		if _, err := p.Lookup(context.Background(), resolver.Query{GitHubLogin: l}); err != nil {
			t.Fatal(err)
		}
	}
	p.mu.Lock()
	n := len(p.cache)
	_, dimsCached := p.cache["dims"]
	p.mu.Unlock()
	if n != 2 || dimsCached {
		t.Errorf("cache size %d, dims cached %v", n, dimsCached)
	}
	// Expired entries are swept first.
	now = now.Add(2 * time.Hour)
	if _, err := p.Lookup(context.Background(), resolver.Query{GitHubLogin: "a3"}); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	n = len(p.cache)
	p.mu.Unlock()
	if n != 1 {
		t.Errorf("expired entries must be swept, size %d", n)
	}
}

func TestErrors(t *testing.T) {
	f := newFake(t)
	p := newProvider(f, Options{Projects: []string{"No Such Group"}})
	_, err := p.Lookup(context.Background(), resolver.Query{GitHubLogin: "dims"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.API != apiDevActCnt || !strings.Contains(apiErr.Error(), "invalid repository_group") {
		t.Errorf("bad project = %v", err)
	}
	f.mu.Lock()
	f.broken = true
	f.mu.Unlock()
	var se *fetch.StatusError
	if _, err := newProvider(f, Options{}).Lookup(context.Background(), resolver.Query{GitHubLogin: "dims"}); !errors.As(err, &se) {
		t.Errorf("outage = %v", err)
	}
	if _, err := newProvider(f, Options{}).Projects(context.Background()); err == nil {
		t.Error("Projects during an outage must fail")
	}
}

func TestErrorInSuccessBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"error":"temporarily unavailable"}`))
	}))
	defer srv.Close()
	p := New(Options{BaseURL: srv.URL, Projects: []string{"Kubernetes"}, Client: fetch.New(fetch.Options{})})
	var apiErr *APIError
	if _, err := p.Lookup(context.Background(), resolver.Query{GitHubLogin: "dims"}); !errors.As(err, &apiErr) {
		t.Errorf("error field in a 200 = %v", err)
	}
	if _, err := p.Projects(context.Background()); !errors.As(err, &apiErr) {
		t.Errorf("Projects error field = %v", err)
	}
	if n, err := p.projectContributions(context.Background(), "Kubernetes", "dims"); !errors.As(err, &apiErr) || n != 0 {
		t.Errorf("projectContributions error field = %d, %v", n, err)
	}
}

func TestProjects(t *testing.T) {
	f := newFake(t)
	got, err := newProvider(f, Options{}).Projects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "Agones,Kubernetes" {
		t.Errorf("projects = %v (the All pseudo-group must be dropped)", got)
	}
}

func TestDefaults(t *testing.T) {
	p := New(Options{})
	if p.opts.BaseURL != DefaultBaseURL || p.opts.Range != DefaultRange || p.opts.CacheTTL != DefaultCacheTTL ||
		p.opts.MaxCacheEntries != DefaultMaxCacheEntries || p.client == nil {
		t.Errorf("defaults = %+v", p.opts)
	}
}
