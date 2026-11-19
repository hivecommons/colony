package fetch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func TestGetSendsGenericUserAgent(t *testing.T) {
	var ua atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua.Store(r.Header.Get("User-Agent"))
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	body, err := New(Options{}).Get(context.Background(), srv.URL)
	if err != nil || string(body) != "ok" {
		t.Fatalf("Get = %q, %v", body, err)
	}
	if got := ua.Load().(string); got != UserAgent() || !strings.HasPrefix(got, "colony/") {
		t.Errorf("User-Agent = %q, want %q", got, UserAgent())
	}
}

func TestRedirectIsRefused(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits.Add(1)
	}))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer srv.Close()
	_, err := New(Options{}).Get(context.Background(), srv.URL)
	if !errors.Is(err, ErrRedirect) {
		t.Fatalf("want ErrRedirect, got %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("redirect target must never be contacted")
	}
}

func TestStatusErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	c := New(Options{})
	if _, err := c.Get(context.Background(), srv.URL+"/missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
	var se *StatusError
	if _, err := c.Get(context.Background(), srv.URL+"/x"); !errors.As(err, &se) || se.Status != http.StatusBadGateway {
		t.Errorf("want StatusError 502, got %v", err)
	}
}

func TestBodyCap(t *testing.T) {
	const limit = 8
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", limit+1)))
	}))
	defer srv.Close()
	if _, err := New(Options{MaxBodyBytes: limit}).Get(context.Background(), srv.URL); err == nil {
		t.Fatal("oversized body must fail")
	}
}

func TestGetCachedETagRoundTrip(t *testing.T) {
	const etag = `"v1"`
	var full, notModified atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == etag {
			notModified.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		full.Add(1)
		w.Header().Set("ETag", etag)
		_, _ = w.Write([]byte("snapshot"))
	}))
	defer srv.Close()
	dir := t.TempDir()
	c := New(Options{CacheDir: dir})
	first, err := c.GetCached(context.Background(), srv.URL)
	if err != nil || string(first.Body) != "snapshot" || first.FromCache || first.ETag != etag {
		t.Fatalf("first = %+v, %v", first, err)
	}
	second, err := c.GetCached(context.Background(), srv.URL)
	if err != nil || string(second.Body) != "snapshot" || !second.FromCache || second.Stale {
		t.Fatalf("second = %+v, %v", second, err)
	}
	if full.Load() != 1 || notModified.Load() != 1 {
		t.Errorf("full=%d notModified=%d", full.Load(), notModified.Load())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != cacheFilePerm {
			t.Errorf("%s perm = %v, want %v", e.Name(), info.Mode().Perm(), os.FileMode(cacheFilePerm))
		}
		if strings.Contains(e.Name(), "tmp") {
			t.Errorf("leftover temp file %s", e.Name())
		}
	}
}

func TestGetCachedServesStaleOnFailureButNotOn404(t *testing.T) {
	var mode atomic.Int32 // 0 ok, 1 fail, 2 not found
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load() {
		case 1:
			w.WriteHeader(http.StatusInternalServerError)
		case 2:
			http.NotFound(w, r)
		default:
			_, _ = w.Write([]byte("v1")) // no ETag: exercises the etag-less store path
		}
	}))
	defer srv.Close()
	c := New(Options{CacheDir: t.TempDir()})
	if _, err := c.GetCached(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
	mode.Store(1)
	resp, err := c.GetCached(context.Background(), srv.URL)
	if err != nil || !resp.Stale || string(resp.Body) != "v1" {
		t.Fatalf("stale = %+v, %v", resp, err)
	}
	mode.Store(2)
	if _, err := c.GetCached(context.Background(), srv.URL); !errors.Is(err, ErrNotFound) {
		t.Fatalf("404 must not be masked, got %v", err)
	}
}

func TestGetCachedWithoutCacheDirAndWithoutCopy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	if _, err := New(Options{}).GetCached(context.Background(), srv.URL); err == nil {
		t.Error("no cache dir: failure must surface")
	}
	if _, err := New(Options{CacheDir: t.TempDir()}).GetCached(context.Background(), srv.URL); err == nil {
		t.Error("empty cache: failure must surface")
	}
}

func TestPostJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("X-Test") != "1" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"echo":"ok"}`))
	}))
	defer srv.Close()
	var out struct {
		Echo string `json:"echo"`
	}
	if err := New(Options{}).PostJSON(context.Background(), srv.URL, map[string]string{"X-Test": "1"}, map[string]string{"a": "b"}, &out); err != nil {
		t.Fatal(err)
	}
	if out.Echo != "ok" {
		t.Errorf("out = %+v", out)
	}
	if err := New(Options{}).PostJSON(context.Background(), srv.URL, nil, nil, nil); err == nil {
		t.Error("400 must fail")
	}
}

func TestRateLimiterHonoursContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	const slow = rate.Limit(0.001) // one request per ~17 minutes
	c := New(Options{RateLimit: slow})
	if _, err := c.Get(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Get(ctx, srv.URL); err == nil {
		t.Fatal("second request must wait on the limiter and give up with the context")
	}
}
