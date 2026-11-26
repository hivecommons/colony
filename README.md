# colony

Contributor affiliation and activity enrichment from public CNCF sources.

colony answers two questions about an open source contributor, given a
GitHub login or an email address:

1. **Where do they work, and since when?** From
   [cncf/gitdm](https://github.com/cncf/gitdm), the curated affiliation list
   that CNCF DevStats uses, and optionally from LFX Insights.
2. **What have they contributed to?** From
   [CNCF DevStats](https://devstats.cncf.io), per project.

It is a Go library plus a small CLI (`colony whois`), and an embedded store
(`store`) that keeps what a consumer knows about people, with the provenance
of every value. It has no dependency on any consumer. [Hive](https://github.com/hivecommons/hive) is its first
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
- `store`: the embedded customer-data store (see [Store](#store)).
- `devstats`: the CNCF DevStats provider. Uses the public DevStats API
  (`GithubIDContributions` for the all-time CNCF-wide total, `DevActCnt` on
  the All CNCF database for each project in `Options.Projects`, over
  `Options.Range`, default `Last year`). `Provider.Projects` lists the valid
  project names (`RepoGroups`). Rate-limited to 1 request per second by
  default; answers are cached in memory for 24 hours. Each fact links to the
  All CNCF developer activity dashboard.

## Store

`store` is an embedded customer-data store over [bbolt](https://github.com/etcd-io/bbolt)
(MIT). It is a library, not a service: it opens no listener and sends nothing
anywhere. Each deployment opens its own file (`colony.db`), and bbolt's file
lock keeps it to one process at a time.

```go
s, err := store.Open("/data/colony.db", store.Options{})
p, err := s.Upsert(store.PersonSpec{Identities: []store.Identity{
	{Kind: store.KindGitHub, Value: "octocat", Verified: true, Locality: store.LocalityShareable},
}})
for _, f := range store.FactsFromResult(res, q, time.Now()) {
	_, err = s.SetFact(p.ID, f) // suggested enrichment, source and confidence intact
}
p, err = s.Get(p.ID)
eff, ok := p.Effective(store.FieldAffiliation)
shared, err := s.Export(store.ShareableFor(hiveID)) // the only export for a path that leaves the deployment
```

- **Schema** (versioned in `meta.schema_version`): a `Person` with a stable id
  the caller may supply; `Identity` (`github`, `ghe`, `email`, `git_email`,
  `slack`, `oidc`) with its provenance; held identities; `Fact` with field,
  value, rank, source, confidence, locality, period, who and when, and a
  `suggested` flag; provider `Answer`s; `Consent` per scope with its
  delivery bookkeeping; attachments; and salted-hash tombstones of
  forgotten identities.
- **Precedence:** one fact per field per rank, and the strongest rank present
  is effective: `user` > `admin` > `hub_shared` > `enrichment`. A write at
  one rank never touches another, so enrichment never overwrites a value a
  person or an administrator set. At the same rank the newer value wins. An
  empty value is an explicit clear. A `hub_shared` source is always labelled
  `hub:<label>`.
- **Suggestions:** an enrichment value is `suggested` until someone accepts
  it. A dismissed value is remembered and not suggested again.
- **Locality:** every identity and fact is `shareable` or `local_only`, and
  an unset locality is stored as `local_only`. An email on any domain that is
  not free-mail, a Slack id and an OIDC subject are always `local_only`, and
  so is every fact derived from a `local_only` identity, plus `notes` and
  `slack_id`.
- **`Shareable()`** is the only export for a path that leaves the deployment.
  It drops every `local_only` value, every email identity, `real_name` and
  `location`, consents, dismissals and who set each value. A `local_only`
  value hides the weaker ranks of its field, and a `local_only` clear is sent
  as a bare clear, so a receiver never sees a value that was overridden or
  cleared. `ShareableFor(scope)` also keeps `real_name` and `location` when
  the person consented for that scope. Person ids are exported, so use opaque
  ids.
- **Forget** erases the person and tombstones the person id and each
  identity, so neither `Upsert`, `Import` nor `Migrate` brings them back.
  bbolt does not zero freed pages; compact the file to scrub them.
- **Migration:** `Import` merges a snapshot (for example an unfiltered
  `Export`) in one transaction, and `Migrate` imports from a consumer's legacy
  store through the `Source` interface, recording its fingerprint once.
  `Stats` returns counts only, for parity logs.
- **Held identities:** an identity has one owner. `Upsert` with
  `HoldConflicts` (and `Replace` always) keeps an identity another person
  owns as a `HeldIdentity` on the claimant, with the owner's id, instead of
  refusing it. A held identity is not in the identity index, so it never
  resolves to the claimant, and it never leaves the deployment.
  `Contested(ref)` names the owner and every holder; `RemoveHeld` drops a
  claim. Forgetting a holder drops its claim without tombstoning the
  owner's identity; `ForgetIdentity` removes it from everyone.
- **Identity provenance:** `manual`, `by` (never exported) and a numeric
  `score` in [0, 1], beside `verified`, `source` and `at`.
- **Provider answers:** `SetAnswer` keeps each provider's last answer in
  full, one per source: every affiliation with its period and every
  activity with its project name, period and link, and when the source
  answered. Facts hold the effective value; answers keep the detail and the
  history. They never leave the deployment.
- **Consent delivery:** `RecordDelivery` stamps the first and last delivery,
  the union of the fields ever delivered, the fields of the last delivery
  and its revision. `DeleteConsent` removes a record.
- **Value limits:** `MaxValueBytes` (4 KiB) per value, `MaxNotesBytes`
  (64 KiB) for `notes`. Values may hold newlines, tabs and carriage returns,
  no other control character. `CheckFactValue` and `ValueLimit` let a
  consumer check a value before writing it.
- **Attachments:** consumer data colony does not interpret, as JSON by
  namespace, per person (`SetAttachment`, at most 1 MiB per person, erased
  by `Forget`) or per store (`SetStoreAttachment`). They are local_only:
  `Shareable` never exports them, and only an unfiltered `Export` carries
  them.
- **Exact writes:** `Replace` sets a person to exactly a `Record` (what the
  stored record holds and the record does not is removed), under the same
  rules as `Upsert` and `SetFact`. `Batch` runs several writes in one
  transaction: all of them commit or none does.
- A file written by a newer colony release fails `Open` with
  `ErrSchemaTooNew` and is left untouched. Schema 2 (v0.4.0) adds the held
  index; `Open` upgrades a schema 1 file in place, and every schema 1
  record reads unchanged. An older release cannot open an upgraded file, so
  take an unfiltered `Export` before going back to one.

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
