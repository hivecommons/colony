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
| `devstats` | The GitHub login only, sent to `devstats.cncf.io`. Never an email address. | opt-in |
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
	"github.com/hivecommons/colony/resolver"
)

r := resolver.New(providerA, providerB) // earlier providers win on conflicts
res, err := r.Lookup(ctx, resolver.Query{GitHubLogin: "octocat"})
for _, a := range res.Affiliations {
	fmt.Println(a.Org, a.From, a.Until, a.Source)
}
```

## CLI

```sh
go install github.com/hivecommons/colony/cmd/colony@latest
colony whois <github-login|email>
colony whois --json <github-login|email>
```

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
