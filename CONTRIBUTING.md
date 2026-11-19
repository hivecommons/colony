# Contributing to colony

Thanks for contributing! A few ground rules.

## Finding something to work on

- Issues labelled [`good first issue`](https://github.com/hivecommons/colony/issues?q=is%3Aissue+is%3Aopen+label%3A%22good+first+issue%22) are scoped for a first PR; [`help wanted`](https://github.com/hivecommons/colony/issues?q=is%3Aissue+is%3Aopen+label%3A%22help+wanted%22) issues are open to anyone.
- Not sure where to start, or want to check an approach first? Ask in the [Hive Commons Discord](https://hivecommons.dev/discord) or bring it to the biweekly [community meeting](https://docs.hivecommons.dev/docs/community/meetings).

## Developer Certificate of Origin (DCO)

All commits **must** be signed off, certifying the
[Developer Certificate of Origin](https://developercertificate.org/):

```sh
git commit -s -m "your message"
```

This adds a `Signed-off-by: Your Name <you@example.com>` trailer. PRs with
unsigned commits fail the `DCO sign-off` check.

## Workflow

1. Fork, or branch from `main`.
2. Make your change with tests.
3. Make sure these pass (CI runs the same):
   - `go build ./...` and `go vet ./...`
   - `go test -race -count=1 ./...`
   - `golangci-lint run`
   - `gofmt -l .` prints nothing, and `go mod tidy` leaves `go.mod` and `go.sum` unchanged
4. Open a PR whose title starts with a kind emoji:

   | Prefix | Kind |
   |---|---|
   | ✨ | feature |
   | 🐛 | bug fix |
   | 📖 | documentation |
   | 🌱 | other, chore, dependencies |
   | ⚠️ | breaking change |
   | 🚀 | release |
   | 🔒 | security |
   | 🧪 | tests only |
   | ♻️ | refactor |

5. Reference the issue with `Fixes #N` in the PR body.
6. Reviews and merges go through Prow: a reviewer adds `/lgtm`, an approver
   (see [OWNERS](OWNERS)) adds `/approve`, and Tide squash-merges once every
   check is green.

## Code conventions

- Standard library first. New dependencies need a reason in the PR.
- No outbound request may send an email address. A provider that contacts a
  remote service per person sends the GitHub login only, and must be listed in
  the README privacy table.
- No magic numbers: name constants, and make tunables configurable.
- Every fact a provider returns carries its `Source`.

## Security

Do not file public issues for security vulnerabilities. See
[SECURITY.md](SECURITY.md) for how to report them privately.
