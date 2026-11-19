# colony

Contributor affiliation and activity enrichment from public CNCF sources.

colony answers two questions about an open source contributor, given a
GitHub login or an email address:

1. **Where do they work, and since when?** From
   [cncf/gitdm](https://github.com/cncf/gitdm), the curated affiliation list
   that CNCF DevStats uses, and optionally from LFX Insights.
2. **What have they contributed to?** From
   [CNCF DevStats](https://devstats.cncf.io), per project.

It is a Go library plus a small CLI (`colony whois`). It has no dependency on
any consumer. [Hive](https://github.com/hivecommons/hive) is its first
consumer, but nothing here knows about Hive.

Status: early. See the [releases](https://github.com/hivecommons/colony/releases)
for what has shipped.

## Privacy model

colony is built so that the default path sends **no per-person data** off the
host, and every outbound path is explicit and opt-in.

| Provider | What leaves the host | Default |
|---|---|---|
| `gitdm` | Nothing per person. colony downloads the public `developers_affiliations*.txt` and alias files from cncf/gitdm over HTTPS GET (the same files for everyone), caches them on disk, and matches logins and emails **locally**. | the provider a consumer enables first |
| `devstats` | The GitHub login only, in JSON POSTs to `https://devstats.cncf.io/api/v1` (one for the CNCF-wide total, plus one per configured project). Never an email address; an email-only query makes no request. | opt-in |
| `crowddev` (LFX Insights) | The GitHub login only, with an API token you supply. Never an email address. | opt-in |

All HTTP traffic goes through one shared client that:

- uses a generic `User-Agent: colony/<version>` with no user, host or consumer identity;
- refuses redirects, so a response can never send a request somewhere else;
- has a request timeout and honours context cancellation;
- rate-limits the per-person providers;
- caches with ETags so unchanged snapshots are not downloaded again.

Each fact colony returns carries its `Source` (for example `gitdm`), so a
consumer can show where it came from and let a person accept or dismiss it.

## Library

```go
import (
	"github.com/hivecommons/colony/gitdm"
	"github.com/hivecommons/colony/resolver"
)

gd := gitdm.New(gitdm.Options{
	CacheDir: "/var/cache/colony", // ETag disk cache for the snapshot files
	Refresh:  24 * time.Hour,      // revalidate the snapshot daily (the default)
})
r := resolver.New(gd) // earlier providers take precedence on conflicts
res, err := r.Lookup(ctx, resolver.Query{GitHubLogin: "octocat"})
for _, a := range res.Affiliations {
	fmt.Println(a.Org, a.From, a.Until, a.Source, a.Confidence)
}
```

Packages:

- `resolver`: the `Provider` interface, the `Query` and `Result` types, and
  the `Resolver` that merges providers and records the source of each fact.
- `fetch`: the shared, guarded HTTP client.
- `gitdm`: the cncf/gitdm provider. Parses `developers_affiliations*.txt`
  (discovered as `developers_affiliations1.txt`, `2`, ... until the first
  404) and `src/cncf-config/aliases`. A GitHub login match is reported with
  `high` confidence, an email match with `medium` (gitdm calls its emails
  "best effort"). gitdm's placeholder values (`NotFound`, `(Unknown)`) are
  not reported as organisations; `Independent` is.
- `devstats`: the CNCF DevStats provider. Uses the public DevStats API
  (`GithubIDContributions` for the all-time CNCF-wide total, `DevActCnt` on
  the All CNCF database for each project in `Options.Projects`, over
  `Options.Range`, default `Last year`). `Provider.Projects` lists the valid
  project names (`RepoGroups`). Rate-limited to 1 request per second by
  default; answers are cached in memory for 24 hours. Each fact links to the
  All CNCF developer activity dashboard.

## CLI

```sh
go install github.com/hivecommons/colony/cmd/colony@latest
colony whois <github-login|email>
colony whois --json <github-login|email>
colony whois --devstats --devstats-projects "Kubernetes,Hive Commons" <github-login>
```

A value containing `@` is matched as an email address; anything else (a
leading `@` is allowed) as a GitHub login. The exit status is 0 when
something was found, 3 when nothing was, 1 on errors and 2 on usage errors.

## Data sources and licences

- cncf/gitdm data is published by CNCF in the
  [cncf/gitdm](https://github.com/cncf/gitdm) repository. colony downloads it
  at run time; it does not redistribute it, apart from small excerpts used as
  test fixtures (see `gitdm/testdata/README.md`).
- People who do not want to be listed in gitdm can follow
  [gitdm's removal process](https://github.com/cncf/gitdm/blob/master/FORBIDDEN_DATA.md).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Security reports: [SECURITY.md](SECURITY.md).

colony is part of [Hive Commons](https://hivecommons.dev), a CNCF Sandbox
project. Licensed under the [Apache License 2.0](LICENSE).
