package store

import (
	"path/filepath"
	"testing"
	"time"
)

// Fixtures use reserved example domains and made-up logins only.
const (
	testLockTimeout = 200 * time.Millisecond
	testFreeMail    = "mail.example"
	loginA          = "fixture-login-a"
	loginB          = "fixture-login-b"
	privateEmail    = "dev@corp.example"
	freeEmail       = "dev@" + testFreeMail
)

var testBase = time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)

// testNow is the store clock: a day after testBase, so fixture timestamps
// a few hours after testBase are in the past, not clamped as future.
var testNow = testBase.Add(24 * time.Hour)

func testOptions() Options {
	return Options{
		LockTimeout:     testLockTimeout,
		FreeMailDomains: []string{testFreeMail},
		Now:             func() time.Time { return testNow },
	}
}

func openAt(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path, testOptions())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func openTest(t *testing.T) *Store {
	t.Helper()
	s := openAt(t, filepath.Join(t.TempDir(), DefaultFileName))
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustUpsert(t *testing.T, s *Store, spec PersonSpec) Person {
	t.Helper()
	p, err := s.Upsert(spec)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	return p
}

func mustSet(t *testing.T, s *Store, id string, f Fact) SetResult {
	t.Helper()
	res, err := s.SetFact(id, f)
	if err != nil {
		t.Fatalf("SetFact(%s/%s): %v", f.Field, f.Rank, err)
	}
	return res
}

func mustGet(t *testing.T, s *Store, id string) Person {
	t.Helper()
	p, err := s.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return p
}

func githubIdentity(login string) Identity {
	return Identity{Kind: KindGitHub, Value: login, Verified: true, Locality: LocalityShareable, Source: "test"}
}

func fact(field, value string, rank Rank) Fact {
	src := "test"
	if rank == RankHubShared {
		src = HubSourcePrefix + "test"
	}
	return Fact{Field: field, Value: value, Rank: rank, Source: src, Locality: LocalityShareable, At: testBase}
}
