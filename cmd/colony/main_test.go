package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/colony/gitdm"
	"github.com/hivecommons/colony/resolver"
)

func gitdmServer(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../../gitdm/testdata/developers_affiliations1.txt")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + gitdm.AffiliationFile(1):
			_, _ = w.Write(data)
		case "/" + gitdm.AliasesFile:
			_, _ = w.Write([]byte("# none\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestWhoisHuman(t *testing.T) {
	base := gitdmServer(t)
	code, out, errOut := runCLI(t, "whois", "--cache-dir", t.TempDir(), "--gitdm-base-url", base, "@dims")
	if code != exitOK {
		t.Fatalf("exit %d, stderr %s", code, errOut)
	}
	for _, want := range []string{"@dims", "NVIDIA Corporation", "2025-07-11", "gitdm", "high", "github_login", "*"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestWhoisJSONByEmail(t *testing.T) {
	base := gitdmServer(t)
	code, out, errOut := runCLI(t, "whois", "--json", "--cache-dir", "", "--gitdm-base-url", base, "thockin@google.com")
	if code != exitOK {
		t.Fatalf("exit %d, stderr %s", code, errOut)
	}
	var got struct {
		Query  resolver.Query  `json:"query"`
		Result resolver.Result `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("bad JSON: %v\n%s", err, out)
	}
	if len(got.Query.Emails) != 1 || got.Query.GitHubLogin != "" {
		t.Errorf("an email argument must be an email query: %+v", got.Query)
	}
	if len(got.Result.Affiliations) != 1 || got.Result.Affiliations[0].Org != "Google LLC" ||
		got.Result.Affiliations[0].MatchedBy != "email" {
		t.Errorf("result = %+v", got.Result)
	}
	if strings.Contains(out, `"from"`) {
		t.Errorf("zero dates must be omitted: %s", out)
	}
}

func TestWhoisNotFoundAndErrors(t *testing.T) {
	base := gitdmServer(t)
	if code, out, _ := runCLI(t, "whois", "--cache-dir", "", "--gitdm-base-url", base, "nobody-here"); code != exitNotFound ||
		!strings.Contains(out, "no affiliation") {
		t.Errorf("not found: exit %d, out %q", code, out)
	}
	if code, _, errOut := runCLI(t, "whois", "--cache-dir", "", "--timeout", time.Second.String(), "--gitdm-base-url", base+"/missing/", "dims"); code != exitError ||
		!strings.Contains(errOut, "gitdm") {
		t.Errorf("fetch failure: exit %d, stderr %q", code, errOut)
	}
}

func TestUsage(t *testing.T) {
	cases := []struct {
		args []string
		code int
	}{
		{nil, exitUsage},
		{[]string{"bogus"}, exitUsage},
		{[]string{"whois"}, exitUsage},
		{[]string{"whois", "a", "b"}, exitUsage},
		{[]string{"whois", "@"}, exitUsage},
		{[]string{"whois", "--no-such-flag", "x"}, exitUsage},
		{[]string{"whois", "-h"}, exitOK},
		{[]string{"help"}, exitOK},
		{[]string{"version"}, exitOK},
	}
	for _, c := range cases {
		if code, _, _ := runCLI(t, c.args...); code != c.code {
			t.Errorf("%v: exit %d, want %d", c.args, code, c.code)
		}
	}
}

func TestPrintHumanActivityAndWarnings(t *testing.T) {
	var b bytes.Buffer
	printHuman(&b, "someone", resolver.Result{
		Activity: []resolver.Activity{{Project: "Kubernetes", Contributions: 3, Period: "y", Source: "devstats", URL: "https://example.org"}},
		Errors:   []resolver.ProviderError{{Source: "x", Error: "down"}},
	}, time.Now())
	for _, want := range []string{"warning: x: down", "Kubernetes", "devstats"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q in %s", want, b.String())
		}
	}
	if defaultCacheDir() == "" && os.Getenv("HOME") != "" {
		t.Error("defaultCacheDir should resolve when HOME is set")
	}
}
