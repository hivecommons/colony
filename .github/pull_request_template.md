## Summary

<!-- What does this PR change, and why? Name any new outbound request (host, method, what identifier it sends). -->

Fixes #<!-- issue number -->

## Validation

<!-- Which commands did you run, and what did you check by hand? -->

## Checklist

- [ ] PR title starts with an emoji kind prefix (see CONTRIBUTING.md)
- [ ] `go vet ./...`, `go test -race ./...` and `golangci-lint run` pass
- [ ] `gofmt -l .` prints nothing and `go mod tidy` leaves `go.mod` and `go.sum` unchanged
- [ ] Any new outbound request is listed in the README privacy table, and never sends an email address
- [ ] Commits are signed off (`git commit -s`)
