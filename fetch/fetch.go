// Package fetch is the single HTTP client every colony provider uses.
//
// It exists so that every outbound request shares the same guarantees:
// a request timeout, no redirects, a generic User-Agent that carries no user,
// host or consumer identity, an optional rate limit, a response size cap,
// context cancellation, and an optional ETag disk cache.
package fetch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/hivecommons/colony"
)

const (
	// DefaultTimeout bounds one request, including reading the body.
	DefaultTimeout = 30 * time.Second
	// DefaultMaxBodyBytes caps a response body. The largest cncf/gitdm
	// affiliation file is about 1 MB.
	DefaultMaxBodyBytes = 32 << 20
	// maxErrorBodyBytes bounds how much of an error response is kept.
	maxErrorBodyBytes = 4 << 10
	// cacheDirPerm and cacheFilePerm keep cached snapshots private to the
	// running user.
	cacheDirPerm  = 0o700
	cacheFilePerm = 0o600
)

// ErrNotFound is returned for an HTTP 404.
var ErrNotFound = errors.New("fetch: not found")

// ErrRedirect is returned when a server answers with a redirect; the client
// never follows one.
var ErrRedirect = errors.New("fetch: redirect refused")

// StatusError is a non-success HTTP status. Body holds the start of the
// response body (at most maxErrorBodyBytes), so callers can read an API's
// JSON error message.
type StatusError struct {
	URL    string
	Status int
	Body   []byte
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("fetch: %s: HTTP %d", e.URL, e.Status)
}

// Options configures a Client. The zero value is usable.
type Options struct {
	// Timeout per request; DefaultTimeout when zero.
	Timeout time.Duration
	// UserAgent overrides the generic "colony/<version>". Leave it empty
	// unless you have a reason; never put user or host identity in it.
	UserAgent string
	// RateLimit is the sustained request rate; zero means unlimited.
	RateLimit rate.Limit
	// Burst is the rate limiter burst; 1 when zero and RateLimit is set.
	Burst int
	// CacheDir enables the ETag disk cache for GetCached. Empty disables it.
	CacheDir string
	// MaxBodyBytes caps a response body; DefaultMaxBodyBytes when zero.
	MaxBodyBytes int64
	// Transport overrides the HTTP transport (tests).
	Transport http.RoundTripper
}

// Client is a guarded HTTP client. It is safe for concurrent use.
type Client struct {
	http     *http.Client
	ua       string
	limiter  *rate.Limiter
	cacheDir string
	maxBody  int64
}

// UserAgent is the generic User-Agent colony sends.
func UserAgent() string {
	return "colony/" + colony.Version
}

// New returns a Client configured by opts.
func New(opts Options) *Client {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	transport := opts.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	c := &Client{
		http: &http.Client{
			Timeout:   timeout,
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		ua:       opts.UserAgent,
		cacheDir: opts.CacheDir,
		maxBody:  opts.MaxBodyBytes,
	}
	if c.ua == "" {
		c.ua = UserAgent()
	}
	if c.maxBody <= 0 {
		c.maxBody = DefaultMaxBodyBytes
	}
	if opts.RateLimit > 0 {
		burst := opts.Burst
		if burst <= 0 {
			burst = 1
		}
		c.limiter = rate.NewLimiter(opts.RateLimit, burst)
	}
	return c
}

// Response is a fetched body.
type Response struct {
	Body []byte
	ETag string
	// FromCache is true when the body came from the disk cache, either
	// because the server answered 304 Not Modified or because the request
	// failed and a cached copy was served instead (then Stale is true too).
	FromCache bool
	Stale     bool
}

func (c *Client) do(ctx context.Context, req *http.Request) (*http.Response, error) {
	if c.limiter != nil {
		if err := c.limiter.Wait(ctx); err != nil {
			return nil, err
		}
	}
	req.Header.Set("User-Agent", c.ua)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= http.StatusMultipleChoices && resp.StatusCode < http.StatusBadRequest &&
		resp.StatusCode != http.StatusNotModified {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%w: %s answered HTTP %d", ErrRedirect, req.URL.Redacted(), resp.StatusCode)
	}
	return resp, nil
}

func (c *Client) readBody(resp *http.Response) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBody+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > c.maxBody {
		return nil, fmt.Errorf("fetch: %s: body exceeds %d bytes", resp.Request.URL.Redacted(), c.maxBody)
	}
	return body, nil
}

func checkStatus(resp *http.Response) error {
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, resp.Request.URL.Redacted())
	case resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return &StatusError{URL: resp.Request.URL.Redacted(), Status: resp.StatusCode, Body: body}
	}
	return nil
}

// Get performs a plain GET without the disk cache.
func (c *Client) Get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := checkStatus(resp); err != nil {
		return nil, err
	}
	return c.readBody(resp)
}

// PostJSON sends in as a JSON body and decodes a JSON answer into out. The
// headers map adds request headers (for example an Authorization token).
func (c *Client) PostJSON(ctx context.Context, url string, headers map[string]string, in, out any) error {
	payload, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.do(ctx, req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := checkStatus(resp); err != nil {
		return err
	}
	body, err := c.readBody(resp)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

// GetCached performs a conditional GET backed by the ETag disk cache. With
// no CacheDir it behaves like Get. When the request fails and a cached copy
// exists, the cached copy is returned with Stale set and a nil error; a 404
// is never masked by the cache.
func (c *Client) GetCached(ctx context.Context, url string) (Response, error) {
	if c.cacheDir == "" {
		body, err := c.Get(ctx, url)
		return Response{Body: body}, err
	}
	bodyPath, etagPath := c.cachePaths(url)
	cachedETag := readSmall(etagPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Response{}, err
	}
	if cachedETag != "" {
		if _, statErr := os.Stat(bodyPath); statErr == nil {
			req.Header.Set("If-None-Match", cachedETag)
		}
	}
	resp, err := c.do(ctx, req)
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, err
		}
		return c.staleOr(bodyPath, cachedETag, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotModified {
		body, readErr := os.ReadFile(bodyPath)
		if readErr != nil {
			return Response{}, fmt.Errorf("fetch: 304 for %s but cache unreadable: %w", req.URL.Redacted(), readErr)
		}
		return Response{Body: body, ETag: cachedETag, FromCache: true}, nil
	}
	if err := checkStatus(resp); err != nil {
		if errors.Is(err, ErrNotFound) {
			return Response{}, err
		}
		return c.staleOr(bodyPath, cachedETag, err)
	}
	body, err := c.readBody(resp)
	if err != nil {
		return c.staleOr(bodyPath, cachedETag, err)
	}
	etag := resp.Header.Get("ETag")
	if err := c.store(bodyPath, etagPath, body, etag); err != nil {
		return Response{}, err
	}
	return Response{Body: body, ETag: etag}, nil
}

func (c *Client) staleOr(bodyPath, etag string, cause error) (Response, error) {
	body, err := os.ReadFile(bodyPath)
	if err != nil {
		return Response{}, cause
	}
	return Response{Body: body, ETag: etag, FromCache: true, Stale: true}, nil
}

func (c *Client) cachePaths(url string) (string, string) {
	sum := sha256.Sum256([]byte(url))
	name := hex.EncodeToString(sum[:])
	return filepath.Join(c.cacheDir, name+".body"), filepath.Join(c.cacheDir, name+".etag")
}

func (c *Client) store(bodyPath, etagPath string, body []byte, etag string) error {
	if err := os.MkdirAll(c.cacheDir, cacheDirPerm); err != nil {
		return err
	}
	if err := writeAtomic(bodyPath, body); err != nil {
		return err
	}
	if etag == "" {
		err := os.Remove(etagPath)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return writeAtomic(etagPath, []byte(etag))
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	cleanup := func() { _ = os.Remove(name) }
	if err := tmp.Chmod(cacheFilePerm); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(name, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// maxETagBytes bounds how much of a cached ETag file is read.
const maxETagBytes = 1024

func readSmall(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxETagBytes))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
