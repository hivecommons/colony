package store

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/colony/resolver"
	bolt "go.etcd.io/bbolt"
)

// Fixtures use reserved example domains and made-up logins only.
const (
	loginC      = "fixture-login-c"
	testAdmin   = "fixture-admin"
	testScope   = "fixture-hive"
	testSource  = "colony:gitdm"
	testProject = "Fixture Project"
)

func TestIdentityProvenance(t *testing.T) {
	s := openTest(t)
	in := githubIdentity(loginA)
	in.Manual, in.By, in.Score = true, testAdmin, 0.75
	mustUpsert(t, s, PersonSpec{ID: "p1", Identities: []Identity{in}})

	got := mustGet(t, s, "p1").Identities[0]
	if !got.Manual || got.By != testAdmin || got.Score != 0.75 {
		t.Fatalf("identity = %+v, want manual, by and score kept", got)
	}

	// A later automatic upsert that names none of them keeps all three.
	mustUpsert(t, s, PersonSpec{ID: "p1", Identities: []Identity{{Kind: KindGitHub, Value: loginA}}})
	got = mustGet(t, s, "p1").Identities[0]
	if !got.Manual || got.By != testAdmin || got.Score != 0.75 {
		t.Errorf("identity after a bare upsert = %+v, want provenance kept", got)
	}

	for _, score := range []float64{-0.1, 1.5, math.NaN(), math.Inf(1)} {
		bad := githubIdentity(loginB)
		bad.Score = score
		if _, err := s.Upsert(PersonSpec{ID: "p2", Identities: []Identity{bad}}); !errors.Is(err, ErrInvalid) {
			t.Errorf("score %v: err = %v, want ErrInvalid", score, err)
		}
	}
	long := githubIdentity(loginB)
	long.By = strings.Repeat("b", maxByBytes+1)
	if _, err := s.Upsert(PersonSpec{ID: "p2", Identities: []Identity{long}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("long by: err = %v, want ErrInvalid", err)
	}

	shared, err := s.Export(Shareable())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range shared.People {
		for _, i := range p.Identities {
			if i.By != "" {
				t.Errorf("shareable export says who linked %s", i.Kind)
			}
		}
	}
}

func TestHoldConflicts(t *testing.T) {
	s := openTest(t)
	mustUpsert(t, s, PersonSpec{ID: "p1", Identities: []Identity{githubIdentity(loginA)}})

	if _, err := s.Upsert(PersonSpec{ID: "p2", Identities: []Identity{githubIdentity(loginA)}}); !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("without HoldConflicts: err = %v, want ErrIdentityConflict", err)
	}
	claim := githubIdentity(loginA)
	claim.By = testAdmin
	p2 := mustUpsert(t, s, PersonSpec{ID: "p2", HoldConflicts: true,
		Identities: []Identity{claim, githubIdentity(loginB)}})
	if len(p2.Identities) != 1 || p2.Identities[0].Value != loginB {
		t.Errorf("p2 owns %+v, want only its own login", p2.Identities)
	}
	if len(p2.Held) != 1 || p2.Held[0].Value != loginA || p2.Held[0].Owner != "p1" || p2.Held[0].By != testAdmin {
		t.Fatalf("p2 held = %+v, want the conflicting login owned by p1, by kept", p2.Held)
	}
	if owner, err := s.FindByIdentity(KindGitHub, loginA); err != nil || owner.ID != "p1" {
		t.Errorf("a held identity resolved to %q, %v; want the owner p1", owner.ID, err)
	}
	c, err := s.Contested(IdentityRef{Kind: KindGitHub, Value: loginA})
	if err != nil || c.Owner != "p1" || !slices.Equal(c.Holders, []string{"p2"}) || !c.Contested() {
		t.Errorf("Contested = %+v, %v; want owner p1 and holder p2", c, err)
	}
	if c, _ := s.Contested(IdentityRef{Kind: KindGitHub, Value: loginB}); c.Contested() {
		t.Errorf("an identity with one claimant is contested: %+v", c)
	}

	// The holder never exports a held identity.
	shared, err := s.Export(Shareable())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range shared.People {
		if len(p.Held) > 0 {
			t.Errorf("shareable export of %s holds a held identity", p.ID)
		}
	}

	// The owner releases it; the holder takes ownership with a plain upsert,
	// and the held copy goes.
	if err := s.RemoveIdentity("p1", IdentityRef{Kind: KindGitHub, Value: loginA}); err != nil {
		t.Fatal(err)
	}
	p2 = mustUpsert(t, s, PersonSpec{ID: "p2", Identities: []Identity{githubIdentity(loginA)}})
	if len(p2.Held) != 0 || !p2.HasIdentity(IdentityRef{Kind: KindGitHub, Value: loginA}) {
		t.Errorf("after taking ownership p2 = %+v", p2)
	}
	if c, _ := s.Contested(IdentityRef{Kind: KindGitHub, Value: loginA}); c.Owner != "p2" || len(c.Holders) != 0 {
		t.Errorf("Contested after the move = %+v", c)
	}

	// Held directly, then dropped with RemoveHeld.
	p3 := mustUpsert(t, s, PersonSpec{ID: "p3", Held: []Identity{githubIdentity(loginA)}})
	if !p3.HoldsIdentity(IdentityRef{Kind: KindGitHub, Value: loginA}) || p3.Held[0].Owner != "p2" {
		t.Fatalf("p3 = %+v, want loginA held, owned by p2", p3)
	}
	if ok, err := s.RemoveHeld("p3", IdentityRef{Kind: KindGitHub, Value: loginA}); err != nil || !ok {
		t.Fatalf("RemoveHeld = %v, %v", ok, err)
	}
	if c, _ := s.Contested(IdentityRef{Kind: KindGitHub, Value: loginA}); len(c.Holders) != 0 {
		t.Errorf("index still lists a dropped holder: %+v", c)
	}
}

func TestForgetHeldIdentity(t *testing.T) {
	s := openTest(t)
	mustUpsert(t, s, PersonSpec{ID: "p1", Identities: []Identity{githubIdentity(loginA)}})
	mustUpsert(t, s, PersonSpec{ID: "p2", Identities: []Identity{githubIdentity(loginA), githubIdentity(loginB)}, HoldConflicts: true})
	mustUpsert(t, s, PersonSpec{ID: "p3", Held: []Identity{githubIdentity(loginA)}})

	// Forgetting a holder drops its claim and leaves the owner alone: the
	// identity is the owner's, so it is not tombstoned.
	if _, err := s.Forget("p3"); err != nil {
		t.Fatal(err)
	}
	ref := IdentityRef{Kind: KindGitHub, Value: loginA}
	if c, _ := s.Contested(ref); c.Owner != "p1" || !slices.Equal(c.Holders, []string{"p2"}) {
		t.Errorf("after forgetting a holder Contested = %+v", c)
	}
	if tomb, err := s.IsTombstoned(ref); err != nil || tomb {
		t.Errorf("forgetting a holder tombstoned the owner's identity: %v, %v", tomb, err)
	}

	// Forgetting the identity takes it from the owner and every holder.
	if err := s.ForgetIdentity(ref); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, s, "p2"); len(got.Held) != 0 {
		t.Errorf("a forgotten identity is still held: %+v", got.Held)
	}
	if c, _ := s.Contested(ref); c.Owner != "" || len(c.Holders) != 0 {
		t.Errorf("a forgotten identity is still claimed: %+v", c)
	}
	if _, err := s.Upsert(PersonSpec{ID: "p2", Held: []Identity{githubIdentity(loginA)}}); !errors.Is(err, ErrTombstoned) {
		t.Errorf("holding a forgotten identity: err = %v, want ErrTombstoned", err)
	}
}

func TestReplaceSetsExactState(t *testing.T) {
	s := openTest(t)
	mustUpsert(t, s, PersonSpec{ID: "owner", Identities: []Identity{githubIdentity(loginC)}})
	mustUpsert(t, s, PersonSpec{ID: "p1", Identities: []Identity{githubIdentity(loginA), githubIdentity(loginB)}})
	mustSet(t, s, "p1", fact(FieldOrganization, "Old Org", RankUser))
	mustSet(t, s, "p1", fact(FieldCountry, "AA", RankEnrichment))
	if err := s.SetConsent("p1", Consent{Scope: "old-scope", Fields: []string{FieldCountry}, GrantedAt: testBase}); err != nil {
		t.Fatal(err)
	}

	rec := Record{
		Person: PersonSpec{ID: "p1", Status: "customer", Identities: []Identity{
			githubIdentity(loginA),
			githubIdentity(loginC), // owned by someone else: held
		}},
		Facts: []Fact{fact(FieldOrganization, "New Org", RankUser), fact(FieldNotes, "note", RankAdmin)},
		Consents: []Consent{{Scope: testScope, Fields: []string{FieldCountry}, GrantedAt: testBase,
			DeliveredAt: testBase, FirstDeliveredAt: testBase, DeliveredFields: []string{FieldCountry}, DeliveredRev: "r1"}},
		Answers:     []Answer{{Source: testSource, FetchedAt: testBase, Affiliations: []resolver.Affiliation{{Org: "New Org", Source: "gitdm"}}}},
		Attachments: map[string]json.RawMessage{"consumer": json.RawMessage(`{"tags":["a"]}`)},
	}
	rep, err := s.Replace(rec)
	if err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if rep.Identities != 1 || rep.Held != 1 || rep.Facts != 2 || rep.SkippedIdentities != 0 {
		t.Errorf("report = %+v", rep)
	}
	p := mustGet(t, s, "p1")
	if p.Status != "customer" || len(p.Identities) != 1 || p.Identities[0].Value != loginA {
		t.Errorf("identities = %+v, status %q", p.Identities, p.Status)
	}
	if len(p.Held) != 1 || p.Held[0].Value != loginC || p.Held[0].Owner != "owner" {
		t.Errorf("held = %+v, want loginC held from owner", p.Held)
	}
	if _, ok := p.FactAt(FieldCountry, RankEnrichment); ok {
		t.Error("a fact the record no longer holds survived Replace")
	}
	if f, _ := p.FactAt(FieldOrganization, RankUser); f.Value != "New Org" {
		t.Errorf("organization = %q", f.Value)
	}
	if _, ok := p.Consent("old-scope"); ok {
		t.Error("a consent the record no longer holds survived Replace")
	}
	if c, ok := p.Consent(testScope); !ok || c.DeliveredRev != "r1" || !c.Delivered() {
		t.Errorf("consent = %+v", c)
	}
	if a, ok := p.Answer(testSource); !ok || a.Affiliations[0].Org != "New Org" {
		t.Errorf("answer = %+v", a)
	}
	if string(p.Attachments["consumer"]) != `{"tags":["a"]}` {
		t.Errorf("attachment = %s", p.Attachments["consumer"])
	}
	// The released login is free again.
	if _, err := s.FindByIdentity(KindGitHub, loginB); !errors.Is(err, ErrNotFound) {
		t.Errorf("released identity still resolves: %v", err)
	}
	if owner, _ := s.FindByIdentity(KindGitHub, loginC); owner.ID != "owner" {
		t.Errorf("Replace took an identity from its owner: %q", owner.ID)
	}

	// Replace is idempotent.
	if _, err := s.Replace(rec); err != nil {
		t.Fatal(err)
	}
	again := mustGet(t, s, "p1")
	again.UpdatedAt = p.UpdatedAt
	a, _ := json.Marshal(again)
	b, _ := json.Marshal(p)
	if string(a) != string(b) {
		t.Errorf("a second Replace changed the record:\n%s\n%s", a, b)
	}

	dup := rec
	dup.Facts = []Fact{fact(FieldOrganization, "x", RankUser), fact(FieldOrganization, "y", RankUser)}
	if _, err := s.Replace(dup); !errors.Is(err, ErrInvalid) {
		t.Errorf("duplicate facts: err = %v, want ErrInvalid", err)
	}
	if _, err := s.Replace(Record{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("no id: err = %v, want ErrInvalid", err)
	}
}

func TestReplaceSkipsForgotten(t *testing.T) {
	s := openTest(t)
	if err := s.ForgetIdentity(IdentityRef{Kind: KindGitHub, Value: loginB}); err != nil {
		t.Fatal(err)
	}
	derived := fact(FieldAffiliation, "Org", RankEnrichment)
	derived.DerivedFrom = []IdentityRef{{Kind: KindGitHub, Value: loginB}}
	rep, err := s.Replace(Record{
		Person: PersonSpec{ID: "p1", Identities: []Identity{githubIdentity(loginA), githubIdentity(loginB)}},
		Facts:  []Fact{derived},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.SkippedIdentities != 1 || rep.SkippedFacts != 1 || rep.Identities != 1 {
		t.Errorf("report = %+v, want the forgotten identity and its fact skipped", rep)
	}
	if _, err := s.Forget("p1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Replace(Record{Person: PersonSpec{ID: "p1"}}); !errors.Is(err, ErrTombstoned) {
		t.Errorf("replacing a forgotten person: err = %v, want ErrTombstoned", err)
	}
}

func TestBatchIsAtomic(t *testing.T) {
	s := openTest(t)
	boom := errors.New("boom")
	err := s.Batch(func(b *Batch) error {
		if _, err := b.Replace(Record{Person: PersonSpec{ID: "p1", Identities: []Identity{githubIdentity(loginA)}}}); err != nil {
			return err
		}
		if err := b.SetStoreAttachment("consumer", json.RawMessage(`{"a":1}`)); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Batch err = %v", err)
	}
	if _, err := s.Get("p1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a failed batch wrote a person: %v", err)
	}
	if _, ok, _ := s.StoreAttachment("consumer"); ok {
		t.Error("a failed batch wrote a store attachment")
	}

	err = s.Batch(func(b *Batch) error {
		if _, err := b.Upsert(PersonSpec{ID: "p1", Identities: []Identity{githubIdentity(loginA)}}); err != nil {
			return err
		}
		if _, err := b.Replace(Record{Person: PersonSpec{ID: "p2", Identities: []Identity{githubIdentity(loginB)}}}); err != nil {
			return err
		}
		got, err := b.Get("p2")
		if err != nil || len(got.Identities) != 1 {
			return errors.New("batch does not see its own write")
		}
		if err := b.RemoveIdentity("p2", IdentityRef{Kind: KindGitHub, Value: loginB}); err != nil {
			return err
		}
		_, err = b.Forget("p1")
		return err
	})
	if err != nil {
		t.Fatalf("Batch: %v", err)
	}
	if _, err := s.Get("p1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("forgotten in a batch: %v", err)
	}
	if got := mustGet(t, s, "p2"); len(got.Identities) != 0 {
		t.Errorf("identity removed in a batch is still held: %+v", got.Identities)
	}
}

func TestAnswers(t *testing.T) {
	s := openTest(t)
	mustUpsert(t, s, PersonSpec{ID: "p1", Identities: []Identity{githubIdentity(loginA)}})
	a := Answer{Source: testSource, FetchedAt: testBase,
		Affiliations: []resolver.Affiliation{
			{Org: "Org One", From: testBase.AddDate(-2, 0, 0), Until: testBase.AddDate(-1, 0, 0), Source: "gitdm", Confidence: resolver.ConfidenceHigh, MatchedBy: "github_login"},
			{Org: "Org Two", From: testBase.AddDate(-1, 0, 0), Source: "gitdm", Confidence: resolver.ConfidenceHigh},
		},
		Activity: []resolver.Activity{{Project: testProject, Contributions: 7, Period: "Last year", URL: "https://devstats.example/d", Source: "devstats"}},
	}
	if ok, err := s.SetAnswer("p1", a); err != nil || !ok {
		t.Fatalf("SetAnswer = %v, %v", ok, err)
	}
	older := a
	older.FetchedAt = testBase.Add(-time.Hour)
	older.Affiliations = nil
	if ok, err := s.SetAnswer("p1", older); err != nil || ok {
		t.Errorf("an older answer: stored = %v, %v; want ignored", ok, err)
	}
	got, ok := mustGet(t, s, "p1").Answer(testSource)
	if !ok || len(got.Affiliations) != 2 || got.Activity[0].Period != "Last year" || got.Activity[0].URL != a.Activity[0].URL {
		t.Fatalf("answer = %+v", got)
	}

	for name, bad := range map[string]Answer{
		"no source":   {},
		"bad url":     {Source: testSource, Activity: []resolver.Activity{{Project: testProject, URL: "javascript:x"}}},
		"no project":  {Source: testSource, Activity: []resolver.Activity{{Contributions: 1}}},
		"negative":    {Source: testSource, Activity: []resolver.Activity{{Project: testProject, Contributions: -1}}},
		"confidence":  {Source: testSource, Affiliations: []resolver.Affiliation{{Org: "x", Confidence: "certain"}}},
		"until first": {Source: testSource, Affiliations: []resolver.Affiliation{{Org: "x", From: testBase, Until: testBase.AddDate(-1, 0, 0)}}},
		"too many":    {Source: testSource, Activity: make([]resolver.Activity, maxAnswerActs+1)},
	} {
		if _, err := s.SetAnswer("p1", bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}

	shared, err := s.Export(Shareable())
	if err != nil {
		t.Fatal(err)
	}
	if len(shared.People[0].Answers) != 0 {
		t.Error("shareable export holds a provider answer")
	}
	if err := s.DeleteAnswer("p1", testSource); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAnswer("p1", testSource); !errors.Is(err, ErrNoAnswer) {
		t.Errorf("second delete: err = %v, want ErrNoAnswer", err)
	}
}

func TestAttachments(t *testing.T) {
	s := openTest(t)
	mustUpsert(t, s, PersonSpec{ID: "p1", Identities: []Identity{githubIdentity(loginA)}})
	if err := s.SetAttachment("p1", "consumer", json.RawMessage(`{"tags":["x"]}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAttachment("p1", "consumer", json.RawMessage(`{not json`)); !errors.Is(err, ErrInvalid) {
		t.Errorf("invalid JSON: err = %v, want ErrInvalid", err)
	}
	big := json.RawMessage(`"` + strings.Repeat("a", MaxAttachmentBytes) + `"`)
	if err := s.SetAttachment("p1", "big", big); !errors.Is(err, ErrInvalid) {
		t.Errorf("oversized attachment: err = %v, want ErrInvalid", err)
	}
	if got := mustGet(t, s, "p1").Attachments; string(got["consumer"]) != `{"tags":["x"]}` || len(got) != 1 {
		t.Errorf("attachments = %v", got)
	}
	if err := s.SetStoreAttachment("consumer", json.RawMessage(`{"salt":"00"}`)); err != nil {
		t.Fatal(err)
	}

	full, err := s.Export()
	if err != nil {
		t.Fatal(err)
	}
	if string(full.Attachments["consumer"]) != `{"salt":"00"}` || len(full.People[0].Attachments) != 1 {
		t.Errorf("unfiltered export = %+v", full)
	}
	shared, err := s.Export(Shareable())
	if err != nil {
		t.Fatal(err)
	}
	if len(shared.Attachments) != 0 || len(shared.People[0].Attachments) != 0 {
		t.Error("shareable export holds attachments")
	}

	// A restore into a fresh store brings both back.
	dst := openTest(t)
	if _, err := dst.Import(context.Background(), full); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, dst, "p1").Attachments; string(got["consumer"]) != `{"tags":["x"]}` {
		t.Errorf("restored attachment = %s", got["consumer"])
	}
	if raw, ok, err := dst.StoreAttachment("consumer"); err != nil || !ok || string(raw) != `{"salt":"00"}` {
		t.Errorf("restored store attachment = %s, %v, %v", raw, ok, err)
	}

	// Removing one, then forgetting the person, leaves nothing.
	if err := s.SetAttachment("p1", "consumer", nil); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, s, "p1").Attachments; got != nil {
		t.Errorf("attachments after removal = %v", got)
	}
	if err := s.SetAttachment("p1", "consumer", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Forget("p1"); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Stats(); st.Attachments != 0 {
		t.Errorf("stats after forget = %+v", st)
	}
	if err := s.SetStoreAttachment("consumer", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.StoreAttachment("consumer"); ok {
		t.Error("a removed store attachment is still there")
	}
}

func TestConsentDelivery(t *testing.T) {
	s := openTest(t)
	mustUpsert(t, s, PersonSpec{ID: "p1"})
	if err := s.SetConsent("p1", Consent{Scope: testScope, Fields: []string{FieldRealName, FieldCountry}, GrantedAt: testBase}); err != nil {
		t.Fatal(err)
	}
	c, _ := mustGet(t, s, "p1").Consent(testScope)
	if !c.UpdatedAt.Equal(testNow) || c.Delivered() {
		t.Errorf("new consent = %+v", c)
	}
	first := testBase.Add(time.Hour)
	if err := s.RecordDelivery("p1", testScope, Delivery{Fields: []string{FieldCountry}, Rev: "r1", At: first}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordDelivery("p1", testScope, Delivery{Fields: []string{FieldRealName}, Rev: "r2"}); err != nil {
		t.Fatal(err)
	}
	c, _ = mustGet(t, s, "p1").Consent(testScope)
	if !c.FirstDeliveredAt.Equal(first) || !c.DeliveredAt.Equal(testNow) {
		t.Errorf("delivery times = %v, %v", c.FirstDeliveredAt, c.DeliveredAt)
	}
	if !slices.Equal(c.DeliveredFields, []string{FieldCountry, FieldRealName}) || !slices.Equal(c.LastDeliveredFields, []string{FieldRealName}) || c.DeliveredRev != "r2" {
		t.Errorf("delivered = %+v", c)
	}
	if err := s.RecordDelivery("p1", "other", Delivery{}); !errors.Is(err, ErrNoConsent) {
		t.Errorf("delivery without consent: err = %v, want ErrNoConsent", err)
	}
	if err := s.RecordDelivery("p1", testScope, Delivery{Rev: strings.Repeat("r", maxSourceBytes+1)}); !errors.Is(err, ErrInvalid) {
		t.Errorf("long rev: err = %v, want ErrInvalid", err)
	}

	if err := s.WithdrawConsent("p1", testScope); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.DeleteConsent("p1", testScope); err != nil || !ok {
		t.Fatalf("DeleteConsent = %v, %v", ok, err)
	}
	if _, ok := mustGet(t, s, "p1").Consent(testScope); ok {
		t.Error("a deleted consent is still there")
	}
	if ok, err := s.DeleteConsent("p1", testScope); err != nil || ok {
		t.Errorf("second DeleteConsent = %v, %v", ok, err)
	}
}

func TestLongValues(t *testing.T) {
	s := openTest(t)
	mustUpsert(t, s, PersonSpec{ID: "p1"})
	note := strings.Repeat("line of a long note\r\n", 1000)
	if len(note) <= MaxValueBytes || len(note) > MaxNotesBytes {
		t.Fatalf("fixture note is %d bytes", len(note))
	}
	if err := CheckFactValue(FieldNotes, note); err != nil {
		t.Errorf("CheckFactValue(notes) = %v", err)
	}
	mustSet(t, s, "p1", fact(FieldNotes, note, RankAdmin))
	if f, _ := mustGet(t, s, "p1").FactAt(FieldNotes, RankAdmin); f.Value != note {
		t.Error("a long note was not kept whole")
	}
	if _, err := s.SetFact("p1", fact(FieldNotes, strings.Repeat("a", MaxNotesBytes+1), RankAdmin)); !errors.Is(err, ErrInvalid) {
		t.Errorf("note over the limit: err = %v, want ErrInvalid", err)
	}
	if _, err := s.SetFact("p1", fact(FieldOrganization, strings.Repeat("a", MaxValueBytes+1), RankUser)); !errors.Is(err, ErrInvalid) {
		t.Errorf("organization over the limit: err = %v, want ErrInvalid", err)
	}
	if ValueLimit(FieldNotes) != MaxNotesBytes || ValueLimit(FieldOrganization) != MaxValueBytes {
		t.Error("ValueLimit")
	}
	if err := CheckFactValue(FieldOrganization, "a\x00b"); !errors.Is(err, ErrInvalid) {
		t.Errorf("NUL: err = %v, want ErrInvalid", err)
	}
	if err := CheckFactValue(FieldOrganization, "a\rb"); err != nil {
		t.Errorf("carriage return: err = %v", err)
	}
}

func TestSchemaUpgradeFromV1(t *testing.T) {
	path := filepath.Join(t.TempDir(), DefaultFileName)
	raw, err := bolt.Open(path, fileMode, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A version 1 file as v0.3.0 wrote it: no held bucket, no new fields.
	v1Person := `{"id":"p1","status":"active","identities":[{"kind":"github","value":"` + loginA +
		`","verified":true,"locality":"shareable"}],"consents":[{"scope":"` + testScope + `","fields":["country"],"delivered_at":"2026-01-02T03:04:05Z"}]}`
	if err := raw.Update(func(tx *bolt.Tx) error {
		if err := upgradeV0toV1(tx); err != nil {
			return err
		}
		if err := tx.Bucket(bucketMeta).Put(keySchemaVersion, []byte("1")); err != nil {
			return err
		}
		if err := tx.Bucket(bucketPeople).Put([]byte("p1"), []byte(v1Person)); err != nil {
			return err
		}
		return tx.Bucket(bucketIdentities).Put(indexKey(IdentityRef{Kind: KindGitHub, Value: loginA}), []byte("p1"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	s := openAt(t, path)
	defer func() { _ = s.Close() }()
	if v, err := s.SchemaVersion(); err != nil || v != SchemaVersion {
		t.Fatalf("upgraded schema = %d, %v", v, err)
	}
	p := mustGet(t, s, "p1")
	if len(p.Identities) != 1 || p.Identities[0].Manual || p.Identities[0].Score != 0 {
		t.Errorf("v1 identity read as %+v", p.Identities)
	}
	if c, ok := p.Consent(testScope); !ok || !c.Delivered() || len(c.DeliveredFields) != 0 {
		t.Errorf("v1 consent read as %+v", c)
	}
	// The held index works on the upgraded file.
	mustUpsert(t, s, PersonSpec{ID: "p2", Identities: []Identity{githubIdentity(loginA)}, HoldConflicts: true})
	if c, err := s.Contested(IdentityRef{Kind: KindGitHub, Value: loginA}); err != nil || c.Owner != "p1" || len(c.Holders) != 1 {
		t.Errorf("Contested on an upgraded file = %+v, %v", c, err)
	}
}

func TestStatsCountsNewSlots(t *testing.T) {
	s := openTest(t)
	mustUpsert(t, s, PersonSpec{ID: "p1", Identities: []Identity{githubIdentity(loginA)}})
	mustUpsert(t, s, PersonSpec{ID: "p2", Held: []Identity{githubIdentity(loginA)}})
	if _, err := s.SetAnswer("p1", Answer{Source: testSource}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAttachment("p2", "consumer", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	st, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if st.Held != 1 || st.Answers != 1 || st.Attachments != 1 || st.Identities != 1 {
		t.Errorf("stats = %+v", st)
	}
}
