// Command colony looks up contributor affiliation and activity from public
// CNCF sources.
//
//	colony whois [flags] <github-login|email>
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/hivecommons/colony"
	"github.com/hivecommons/colony/devstats"
	"github.com/hivecommons/colony/fetch"
	"github.com/hivecommons/colony/gitdm"
	"github.com/hivecommons/colony/resolver"
)

const (
	exitOK       = 0
	exitError    = 1
	exitUsage    = 2
	exitNotFound = 3

	cacheSubdir = "colony"
	dateLayout  = time.DateOnly
	openEnded   = "-"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func usage(w io.Writer) {
	_, _ = fmt.Fprintf(w, `colony %s: contributor affiliation and activity from public CNCF sources

Usage:
  colony whois [flags] <github-login|email>
  colony version

Run "colony whois -h" for flags.
`, colony.Version)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	switch args[0] {
	case "whois":
		return whois(ctx, args[1:], stdout, stderr)
	case "version", "--version", "-version":
		_, _ = fmt.Fprintln(stdout, colony.Version)
		return exitOK
	case "help", "-h", "--help":
		usage(stdout)
		return exitOK
	default:
		_, _ = fmt.Fprintf(stderr, "colony: unknown command %q\n", args[0])
		usage(stderr)
		return exitUsage
	}
}

func defaultCacheDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, cacheSubdir)
}

func whois(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("whois", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the merged result as JSON")
	cacheDir := fs.String("cache-dir", defaultCacheDir(), "snapshot cache directory (empty disables the disk cache)")
	timeout := fs.Duration("timeout", fetch.DefaultTimeout, "per-request timeout")
	gitdmBase := fs.String("gitdm-base-url", gitdm.DefaultBaseURL, "cncf/gitdm raw-content root (a mirror may be used)")
	useDevstats := fs.Bool("devstats", false, "also ask CNCF DevStats for contribution activity (sends the GitHub login, never an email)")
	devstatsProjects := fs.String("devstats-projects", "", "comma-separated DevStats project names to break activity down by, e.g. \"Kubernetes,Hive Commons\"")
	devstatsRange := fs.String("devstats-range", devstats.DefaultRange, "DevStats range for the per-project breakdown")
	devstatsBase := fs.String("devstats-base-url", devstats.DefaultBaseURL, "DevStats API endpoint")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "Usage: colony whois [flags] <github-login|email>")
		_, _ = fmt.Fprintln(stderr, "\nA value containing \"@\" is matched as an email address, anything else as a GitHub login.")
		_, _ = fmt.Fprintln(stderr, "gitdm data is downloaded once, cached, and matched locally: the login or email never leaves this host.")
		_, _ = fmt.Fprintln(stderr, "--devstats is opt-in and sends the GitHub login (never an email) to devstats.cncf.io.")
		_, _ = fmt.Fprintln(stderr, "\nFlags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return exitUsage
	}
	q := queryFor(fs.Arg(0))
	if q.Empty() {
		fs.Usage()
		return exitUsage
	}

	client := fetch.New(fetch.Options{Timeout: *timeout, CacheDir: *cacheDir})
	providers := []resolver.Provider{gitdm.New(gitdm.Options{BaseURL: *gitdmBase, Client: client})}
	if *useDevstats {
		providers = append(providers, devstats.New(devstats.Options{
			BaseURL:  *devstatsBase,
			Projects: splitList(*devstatsProjects),
			Range:    *devstatsRange,
			Client: fetch.New(fetch.Options{
				Timeout:   *timeout,
				RateLimit: devstats.DefaultRateLimit,
				Burst:     devstats.DefaultBurst,
			}),
		}))
	}
	r := resolver.New(providers...)
	res, err := r.Lookup(ctx, q)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "colony: %v\n", err)
		return exitError
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(struct {
			Query  resolver.Query  `json:"query"`
			Result resolver.Result `json:"result"`
		}{q, res}); err != nil {
			_, _ = fmt.Fprintf(stderr, "colony: %v\n", err)
			return exitError
		}
	} else {
		printHuman(stdout, fs.Arg(0), res, time.Now())
	}
	if res.Empty() {
		return exitNotFound
	}
	return exitOK
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func queryFor(arg string) resolver.Query {
	arg = strings.TrimSpace(arg)
	if login, ok := strings.CutPrefix(arg, "@"); ok && !strings.Contains(login, "@") {
		return resolver.Query{GitHubLogin: login}
	}
	if strings.Contains(arg, "@") {
		return resolver.Query{Emails: []string{arg}}
	}
	return resolver.Query{GitHubLogin: arg}
}

func formatDate(t time.Time) string {
	if t.IsZero() {
		return openEnded
	}
	return t.Format(dateLayout)
}

func printHuman(w io.Writer, who string, res resolver.Result, now time.Time) {
	for _, pe := range res.Errors {
		_, _ = fmt.Fprintf(w, "warning: %s: %s\n", pe.Source, pe.Error)
	}
	if res.Empty() {
		_, _ = fmt.Fprintf(w, "%s: no affiliation or activity found\n", who)
		return
	}
	_, _ = fmt.Fprintf(w, "%s\n", who)
	if len(res.Affiliations) > 0 {
		_, _ = fmt.Fprintln(w, "\nAffiliations (* = current):")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "  \tORG\tFROM\tUNTIL\tSOURCE\tCONFIDENCE\tMATCHED BY")
		for _, a := range res.Affiliations {
			mark := " "
			if a.CurrentAt(now) {
				mark = "*"
			}
			_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\n", mark, a.Org, formatDate(a.From), formatDate(a.Until), a.Source, a.Confidence, a.MatchedBy)
		}
		_ = tw.Flush()
	}
	if len(res.Activity) > 0 {
		_, _ = fmt.Fprintln(w, "\nActivity:")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "  PROJECT\tCONTRIBUTIONS\tPERIOD\tSOURCE\tURL")
		for _, a := range res.Activity {
			_, _ = fmt.Fprintf(tw, "  %s\t%d\t%s\t%s\t%s\n", a.Project, a.Contributions, a.Period, a.Source, a.URL)
		}
		_ = tw.Flush()
	}
}
