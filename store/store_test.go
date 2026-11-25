package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestUpsertMergeAndConflict(t *testing.T) {
	s := openTest(t)
	p := mustUpsert(t, s, PersonSpec{ID: "p1", Identities: []Identity{githubIdentity(loginA)}})
	if p.Status != StatusActive || !p.CreatedAt.Equal(testNow) {
		t.Errorf("new person = %+v", p)
	}

	// No id: the person holding one of the identities is reused.
	merged := mustUpsert(t, s, PersonSpec{Identities: []Identity{
		{Kind: KindGitHub, Value: "@" + loginA},
		{Kind: KindEmail, Value: privateEmail, Source: "test"},
	}})
	if merged.ID != p.ID || len(merged.Identities) != 2 {
		t.Fatalf("merged = %+v, want p1 with two identities", merged)
	}
	if !merged.Identities[0].Verified {
		t.Error("a later unverified upsert dropped the verified flag")
	}

	other := mustUpsert(t, s, PersonSpec{Identities: []Identity{githubIdentity(loginB)}})
	if other.ID == p.ID || other.ID == "" {
		t.Fatalf("new person id = %q", other.ID)
	}
	if _, err := s.Upsert(PersonSpec{ID: other.ID, Identities: []Identity{githubIdentity(loginA)}}); !errors.Is(err, ErrIdentityConflict) {
		t.Errorf("claiming another person's identity: err = %v, want ErrIdentityConflict", err)
	}
	if _, err := s.Upsert(PersonSpec{Identities: []Identity{githubIdentity(loginA), githubIdentity(loginB)}}); !errors.Is(err, ErrIdentityConflict) {
		t.Errorf("identities of two people: err = %v, want ErrIdentityConflict", err)
	}
	if got := mustGet(t, s, other.ID); len(got.Identities) != 1 {
		t.Errorf("a failed upsert wrote: %+v", got)
	}
	if _, err := s.Upsert(PersonSpec{ID: "p1", Identities: []Identity{{Kind: "fax", Value: "1"}}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown kind: err = %v, want ErrInvalid", err)
	}
	if _, err := s.Upsert(PersonSpec{ID: "p1", Identities: []Identity{{Kind: KindEmail, Value: "no-at-sign"}}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("email without @: err = %v, want ErrInvalid", err)
	}
	if _, err := s.UpsertIdentity("missing", githubIdentity("fixture-login-c")); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpsertIdentity on unknown person: err = %v, want ErrNotFound", err)
	}
	if _, err := s.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get unknown: err = %v, want ErrNotFound", err)
	}
}

func TestForget(t *testing.T) {
	s := openTest(t)
	p := mustUpsert(t, s, PersonSpec{ID: "p1", Identities: []Identity{
		githubIdentity(loginA),
		{Kind: KindGitEmail, Value: privateEmail},
	}})
	mustSet(t, s, p.ID, fact(FieldRealName, "Test Person", RankUser))
	if err := s.SetConsent(p.ID, Consent{Scope: "hub", Fields: []string{FieldRealName}, GrantedAt: testBase}); err != nil {
		t.Fatalf("SetConsent: %v", err)
	}
	keep := mustUpsert(t, s, PersonSpec{ID: "p2", Identities: []Identity{githubIdentity(loginB)}})

	n, err := s.Forget(p.ID)
	if err != nil || n != 2 {
		t.Fatalf("Forget = %d, %v, want 2 tombstones", n, err)
	}
	if _, err := s.Get(p.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Forget: err = %v, want ErrNotFound", err)
	}
	if _, err := s.FindByIdentity(KindGitHub, loginA); !errors.Is(err, ErrNotFound) {
		t.Errorf("FindByIdentity after Forget: err = %v, want ErrNotFound", err)
	}
	// The address was a git_email; it stays forgotten as an email too.
	for _, ref := range []IdentityRef{{KindGitHub, loginA}, {KindEmail, privateEmail}, {KindGitEmail, privateEmail}} {
		if tomb, err := s.IsTombstoned(ref); err != nil || !tomb {
			t.Errorf("IsTombstoned(%v) = %v, %v", ref, tomb, err)
		}
		if _, err := s.Upsert(PersonSpec{Identities: []Identity{{Kind: ref.Kind, Value: ref.Value}}}); !errors.Is(err, ErrTombstoned) {
			t.Errorf("re-adding %v: err = %v, want ErrTombstoned", ref, err)
		}
	}
	snap, err := s.Export()
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	// Two identities plus the person id.
	if len(snap.People) != 1 || snap.People[0].ID != keep.ID || len(snap.Tombstones) != 3 {
		t.Errorf("export after Forget = %d people, %d tombstones", len(snap.People), len(snap.Tombstones))
	}
	if _, err := s.Forget(p.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Forget: err = %v, want ErrNotFound", err)
	}

	// Import never re-adds a forgotten identity.
	rep, err := s.Import(context.Background(), Snapshot{SchemaVersion: SchemaVersion, People: []Person{{
		ID:         "p3",
		Identities: []Identity{githubIdentity(loginA)},
		Facts:      []Fact{{Field: FieldCountry, Value: "AA", Rank: RankUser, Source: "test", DerivedFrom: []IdentityRef{{KindGitHub, loginA}}}},
	}}})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if rep.SkippedIdentities != 1 || rep.SkippedFacts != 1 {
		t.Errorf("import report = %+v, want the identity and its fact skipped", rep)
	}
	if _, err := s.FindByIdentity(KindGitHub, loginA); !errors.Is(err, ErrNotFound) {
		t.Error("Import re-added a forgotten identity")
	}
	if _, err := s.Get("p3"); !errors.Is(err, ErrNotFound) {
		t.Error("Import created a person whose every identity was forgotten")
	}

	// Nor does it bring back the forgotten person id, say from an old backup.
	rep, err = s.Import(context.Background(), Snapshot{SchemaVersion: SchemaVersion, People: []Person{{
		ID:    p.ID,
		Facts: []Fact{fact(FieldRealName, "Test Person", RankUser)},
	}}})
	if err != nil || rep.People != 0 || rep.SkippedFacts != 1 {
		t.Errorf("re-importing a forgotten id = %+v, %v", rep, err)
	}
	if _, err := s.Get(p.ID); !errors.Is(err, ErrNotFound) {
		t.Error("Import brought back a forgotten person")
	}
	if _, err := s.Upsert(PersonSpec{ID: p.ID}); !errors.Is(err, ErrTombstoned) {
		t.Errorf("Upsert of a forgotten id: err = %v, want ErrTombstoned", err)
	}
}

func TestForgetIdentity(t *testing.T) {
	s := openTest(t)
	p := mustUpsert(t, s, PersonSpec{ID: "p1", Identities: []Identity{githubIdentity(loginA), githubIdentity(loginB)}})
	f := fact(FieldAffiliation, "Org One", RankEnrichment)
	f.DerivedFrom = []IdentityRef{{Kind: KindGitHub, Value: loginB}}
	mustSet(t, s, p.ID, f)
	mustSet(t, s, p.ID, fact(FieldCountry, "AA", RankUser))
	if err := s.ForgetIdentity(IdentityRef{Kind: KindGitHub, Value: loginB}); err != nil {
		t.Fatalf("ForgetIdentity: %v", err)
	}
	got := mustGet(t, s, p.ID)
	if len(got.Identities) != 1 || got.HasIdentity(IdentityRef{Kind: KindGitHub, Value: loginB}) {
		t.Errorf("identities after ForgetIdentity = %+v", got.Identities)
	}
	if _, ok := got.Effective(FieldAffiliation); ok {
		t.Error("a fact derived from the forgotten identity survived")
	}
	if _, ok := got.Effective(FieldCountry); !ok {
		t.Error("an unrelated fact was removed")
	}
	if err := s.ForgetIdentity(IdentityRef{Kind: KindEmail, Value: "nobody@corp.example"}); err != nil {
		t.Errorf("forgetting an unknown identity: %v", err)
	}
	if _, err := s.Upsert(PersonSpec{Identities: []Identity{{Kind: KindEmail, Value: "Nobody@Corp.Example"}}}); !errors.Is(err, ErrTombstoned) {
		t.Errorf("re-adding a forgotten unknown identity: err = %v, want ErrTombstoned", err)
	}
}

func TestConcurrentWrites(t *testing.T) {
	s := openTest(t)
	p := mustUpsert(t, s, PersonSpec{ID: "shared"})
	const writers = 16
	const perWriter = 8
	const callsPerStep = 3
	var wg sync.WaitGroup
	errs := make(chan error, writers*perWriter*callsPerStep)
	for w := range writers {
		wg.Go(func() {
			for i := range perWriter {
				step := w*perWriter + i
				f := Fact{Field: FieldCountry, Value: strconv.Itoa(step), Rank: RankEnrichment, Source: "test",
					At: testBase.Add(time.Duration(step) * time.Second)}
				if _, err := s.SetFact(p.ID, f); err != nil {
					errs <- err
				}
				if _, err := s.Upsert(PersonSpec{ID: fmt.Sprintf("w%d-%d", w, i), Identities: []Identity{
					{Kind: KindGitHub, Value: fmt.Sprintf("fixture-%d-%d", w, i)},
				}}); err != nil {
					errs <- err
				}
				if _, err := s.Get(p.ID); err != nil {
					errs <- err
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent call: %v", err)
	}
	// Writes at one rank race, but the newest At always wins.
	got, _ := mustGet(t, s, p.ID).Effective(FieldCountry)
	if want := strconv.Itoa(writers*perWriter - 1); got.Value != want {
		t.Errorf("effective after concurrent writes = %q, want %q", got.Value, want)
	}
	st, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if st.People != writers*perWriter+1 || st.Identities != writers*perWriter || st.Facts[RankEnrichment] != 1 {
		t.Errorf("stats = %+v", st)
	}
}

func TestFutureTimestampIsClamped(t *testing.T) {
	s := openTest(t)
	p := mustUpsert(t, s, PersonSpec{ID: "p1"})
	future := fact(FieldCountry, "AA", RankUser)
	future.At = testNow.Add(MaxClockSkew + time.Hour)
	mustSet(t, s, p.ID, future)
	if f, _ := mustGet(t, s, p.ID).FactAt(FieldCountry, RankUser); !f.At.Equal(testNow) {
		t.Errorf("future At stored as %v, want clamped to %v", f.At, testNow)
	}
	// A correction at the current time still lands.
	now := fact(FieldCountry, "BB", RankUser)
	now.At = testNow
	if res := mustSet(t, s, p.ID, now); !res.Stored {
		t.Errorf("correction after a future write = %+v, want stored", res)
	}
}

func TestReopenPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), DefaultFileName)
	s := openAt(t, path)
	p := mustUpsert(t, s, PersonSpec{ID: "p1", Identities: []Identity{githubIdentity(loginA)}})
	mustSet(t, s, p.ID, fact(FieldCountry, "AA", RankUser))
	if _, err := s.Forget(mustUpsert(t, s, PersonSpec{ID: "p2", Identities: []Identity{githubIdentity(loginB)}}).ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openAt(t, path)
	defer func() { _ = s.Close() }()
	if f, ok := mustGet(t, s, "p1").Effective(FieldCountry); !ok || f.Value != "AA" {
		t.Errorf("after reopen: %+v, %v", f, ok)
	}
	if tomb, err := s.IsTombstoned(IdentityRef{Kind: KindGitHub, Value: loginB}); err != nil || !tomb {
		t.Errorf("tombstone lost on reopen (the salt must persist): %v, %v", tomb, err)
	}
}

func TestSecondOpenIsLocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), DefaultFileName)
	s := openAt(t, path)
	defer func() { _ = s.Close() }()
	if _, err := Open(path, testOptions()); !errors.Is(err, ErrLocked) {
		t.Errorf("second Open: err = %v, want ErrLocked", err)
	}
}

func TestImportIsAllOrNothing(t *testing.T) {
	s := openTest(t)
	snap := Snapshot{SchemaVersion: SchemaVersion, People: []Person{
		{ID: "good", Identities: []Identity{githubIdentity(loginA)}, Facts: []Fact{fact(FieldCountry, "AA", RankUser)}},
		{ID: "bad", Identities: []Identity{{Kind: "fax", Value: "1"}}},
	}}
	if _, err := s.Import(context.Background(), snap); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Import: err = %v, want ErrInvalid", err)
	}
	st, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if st.People != 0 || st.Identities != 0 {
		t.Errorf("a failed import left %+v behind", st)
	}
	if _, err := s.FindByIdentity(KindGitHub, loginA); !errors.Is(err, ErrNotFound) {
		t.Error("a failed import left the identity index behind")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Import(ctx, snap); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled Import: err = %v, want context.Canceled", err)
	}
	if _, err := s.Import(context.Background(), Snapshot{SchemaVersion: SchemaVersion + 1}); !errors.Is(err, ErrSchemaTooNew) {
		t.Errorf("newer snapshot: err = %v, want ErrSchemaTooNew", err)
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	src := openTest(t)
	p := mustUpsert(t, src, PersonSpec{ID: "p1", Identities: []Identity{githubIdentity(loginA), {Kind: KindEmail, Value: privateEmail}}})
	mustSet(t, src, p.ID, fact(FieldCountry, "AA", RankUser))
	mustSet(t, src, p.ID, fact(FieldCountry, "BB", RankEnrichment))
	if err := src.SetConsent(p.ID, Consent{Scope: "hub", Fields: []string{FieldRealName}, GrantedAt: testBase}); err != nil {
		t.Fatal(err)
	}
	mustUpsert(t, src, PersonSpec{ID: "gone", Identities: []Identity{githubIdentity(loginB)}})
	if _, err := src.Forget("gone"); err != nil {
		t.Fatal(err)
	}
	snap, err := src.Export()
	if err != nil {
		t.Fatal(err)
	}

	dst := openTest(t)
	rep, err := dst.Import(context.Background(), snap)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if rep.People != 1 || rep.Facts != 2 || rep.Consents != 1 || rep.Tombstones != 2 {
		t.Errorf("import report = %+v", rep)
	}
	got := mustGet(t, dst, p.ID)
	if eff, _ := got.Effective(FieldCountry); eff.Value != "AA" || len(got.Identities) != 2 {
		t.Errorf("imported person = %+v", got)
	}
	if tomb, err := dst.IsTombstoned(IdentityRef{Kind: KindGitHub, Value: loginB}); err != nil || !tomb {
		t.Errorf("tombstone not carried over: %v, %v", tomb, err)
	}
	// A non-empty store with its own salt cannot take foreign hashes.
	other := openTest(t)
	mustUpsert(t, other, PersonSpec{ID: "x"})
	if _, err := other.Import(context.Background(), snap); !errors.Is(err, ErrSaltMismatch) {
		t.Errorf("foreign tombstones: err = %v, want ErrSaltMismatch", err)
	}
	// Importing the same snapshot again changes nothing.
	if _, err := dst.Import(context.Background(), snap); err != nil {
		t.Fatalf("second Import: %v", err)
	}
	if st, _ := dst.Stats(); st.People != 1 || st.Facts[RankUser] != 1 || st.Facts[RankEnrichment] != 1 {
		t.Errorf("stats after a repeated import = %+v", st)
	}
}

type fakeSource struct {
	fingerprint string
	records     []Record
	failAfter   int // 0 means never
}

func (f fakeSource) Fingerprint() (string, error) { return f.fingerprint, nil }

func (f fakeSource) Records(_ context.Context, fn func(Record) error) error {
	for i, r := range f.records {
		if f.failAfter > 0 && i == f.failAfter {
			return errors.New("legacy file is truncated")
		}
		if err := fn(r); err != nil {
			return err
		}
	}
	return nil
}

func TestMigrate(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	records := []Record{
		{
			Person:    PersonSpec{ID: "legacy-1", Status: "confirmed", Identities: []Identity{githubIdentity(loginA)}},
			Facts:     []Fact{fact(FieldOrganization, "Org One", RankUser), {Field: FieldAffiliation, Value: "Org Two", Rank: RankEnrichment, Source: "colony:gitdm", Suggested: true}},
			Dismissed: []Dismissal{{Field: FieldAffiliation, Value: "Org Old"}},
			CreatedAt: testBase.Add(-time.Hour),
		},
		{Person: PersonSpec{ID: "legacy-2", Identities: []Identity{githubIdentity(loginB)}}, Forgotten: []IdentityRef{{Kind: KindEmail, Value: "former@corp.example"}}},
	}

	broken := fakeSource{fingerprint: "sha256:aaa", records: records, failAfter: 1}
	if _, err := s.Migrate(ctx, "people", broken, MigrateOptions{}); err == nil {
		t.Fatal("Migrate over a failing source succeeded")
	}
	if st, _ := s.Stats(); st.People != 0 {
		t.Errorf("a failed migration left %d people", st.People)
	}
	if _, ran, _ := s.Migration("people"); ran {
		t.Error("a failed migration was recorded")
	}

	src := fakeSource{fingerprint: "sha256:aaa", records: records}
	res, err := s.Migrate(ctx, "people", src, MigrateOptions{})
	if err != nil || res.Skipped {
		t.Fatalf("Migrate = %+v, %v", res, err)
	}
	if res.Record.Fingerprint != "sha256:aaa" || res.Record.Report.People != 2 || res.Record.Report.Tombstones != 1 {
		t.Errorf("migration record = %+v", res.Record)
	}
	got := mustGet(t, s, "legacy-1")
	if got.Status != "confirmed" || !got.CreatedAt.Equal(testBase.Add(-time.Hour)) || len(got.Dismissed) != 1 {
		t.Errorf("migrated person = %+v", got)
	}
	if res := mustSet(t, s, "legacy-1", Fact{Field: FieldAffiliation, Value: "Org Old", Rank: RankEnrichment, Source: "colony:gitdm", Suggested: true}); res.Reason != ReasonDismissed {
		t.Errorf("a migrated dismissal is not honoured: %+v", res)
	}
	if tomb, _ := s.IsTombstoned(IdentityRef{Kind: KindEmail, Value: "former@corp.example"}); !tomb {
		t.Error("a migrated tombstone is missing")
	}

	again, err := s.Migrate(ctx, "people", src, MigrateOptions{})
	if err != nil || !again.Skipped || again.FingerprintChanged {
		t.Errorf("second Migrate = %+v, %v, want skipped", again, err)
	}
	changed := fakeSource{fingerprint: "sha256:bbb", records: records}
	if res, err := s.Migrate(ctx, "people", changed, MigrateOptions{}); err != nil || !res.Skipped || !res.FingerprintChanged {
		t.Errorf("changed source = %+v, %v, want skipped with FingerprintChanged", res, err)
	}
	if res, err := s.Migrate(ctx, "people", changed, MigrateOptions{Reimport: true}); err != nil || res.Skipped || res.Record.Fingerprint != "sha256:bbb" {
		t.Errorf("reimport = %+v, %v", res, err)
	}
	if st, _ := s.Stats(); st.People != 2 || st.Facts[RankUser] != 1 {
		t.Errorf("stats after reimport = %+v", st)
	}
}

func TestSchemaVersion(t *testing.T) {
	if len(upgrades) != SchemaVersion {
		t.Fatalf("%d upgrades for schema version %d", len(upgrades), SchemaVersion)
	}
	s := openTest(t)
	if v, err := s.SchemaVersion(); err != nil || v != SchemaVersion {
		t.Errorf("new file schema = %d, %v", v, err)
	}
}

func TestSchemaUpgradeFromV0(t *testing.T) {
	path := filepath.Join(t.TempDir(), DefaultFileName)
	raw, err := bolt.Open(path, fileMode, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucket([]byte("unrelated"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	s := openAt(t, path)
	defer func() { _ = s.Close() }()
	if v, err := s.SchemaVersion(); err != nil || v != SchemaVersion {
		t.Errorf("upgraded schema = %d, %v", v, err)
	}
	mustUpsert(t, s, PersonSpec{ID: "p1", Identities: []Identity{githubIdentity(loginA)}})
}

func TestNewerSchemaFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), DefaultFileName)
	s := openAt(t, path)
	mustUpsert(t, s, PersonSpec{ID: "p1"})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := bolt.Open(path, fileMode, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketMeta).Put(keySchemaVersion, []byte(strconv.Itoa(SchemaVersion+1)))
	}); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, testOptions()); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("Open newer file: err = %v, want ErrSchemaTooNew", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("opening a newer file rewrote it")
	}
}
