package store

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/hivecommons/colony/resolver"
)

func TestIdentityLocality(t *testing.T) {
	s := openTest(t)
	p := mustUpsert(t, s, PersonSpec{ID: "p1", Identities: []Identity{
		{Kind: KindEmail, Value: privateEmail, Locality: LocalityShareable},
		{Kind: KindGitEmail, Value: "Other@Corp.Example", Locality: LocalityShareable},
		{Kind: KindEmail, Value: freeEmail, Locality: LocalityShareable},
		{Kind: KindGitHub, Value: "@" + loginA, Locality: LocalityShareable},
		{Kind: KindSlack, Value: "U0000TEST"},
	}})
	want := map[string]Locality{
		privateEmail:         LocalityLocalOnly,
		"other@corp.example": LocalityLocalOnly,
		freeEmail:            LocalityShareable,
		loginA:               LocalityShareable,
		"U0000TEST":          LocalityLocalOnly,
	}
	for _, i := range p.Identities {
		if got := want[i.Value]; got != i.Locality {
			t.Errorf("%s %q: locality %q, want %q", i.Kind, i.Value, i.Locality, got)
		}
	}
	if _, err := s.FindByIdentity(KindGitHub, "@"+"FIXTURE-LOGIN-A"); err != nil {
		t.Errorf("login lookup is not case insensitive: %v", err)
	}
	if _, err := s.FindByIdentity(KindEmail, "other@corp.example"); err != nil {
		t.Errorf("a git_email is not found as an email: %v", err)
	}
}

func TestAlwaysLocalFields(t *testing.T) {
	s := openTest(t)
	p := mustUpsert(t, s, PersonSpec{ID: "p1"})
	for _, field := range alwaysLocalFields {
		mustSet(t, s, p.ID, fact(field, "value", RankAdmin))
		if f, _ := mustGet(t, s, p.ID).FactAt(field, RankAdmin); f.Locality != LocalityLocalOnly {
			t.Errorf("%s stored as %q, want local_only", field, f.Locality)
		}
	}
	mustSet(t, s, p.ID, Fact{Field: FieldCountry, Value: "AA", Rank: RankUser, Source: "test"})
	if f, _ := mustGet(t, s, p.ID).FactAt(FieldCountry, RankUser); f.Locality != LocalityLocalOnly {
		t.Errorf("a fact without locality stored as %q, want local_only", f.Locality)
	}
}

func TestShareableNeverLeaksLocalOnly(t *testing.T) {
	s := openTest(t)
	p := mustUpsert(t, s, PersonSpec{ID: "p1", Identities: []Identity{
		githubIdentity(loginA),
		{Kind: KindEmail, Value: privateEmail, Locality: LocalityShareable},
		{Kind: KindEmail, Value: freeEmail, Locality: LocalityShareable},
	}})
	derived := func(field string, ref IdentityRef) Fact {
		f := fact(field, "value", RankEnrichment)
		f.DerivedFrom = []IdentityRef{ref}
		return f
	}
	mustSet(t, s, p.ID, derived("from_login", IdentityRef{Kind: KindGitHub, Value: loginA}))
	mustSet(t, s, p.ID, derived("from_private_email", IdentityRef{Kind: KindEmail, Value: privateEmail}))
	mustSet(t, s, p.ID, derived("from_private_git_email", IdentityRef{Kind: KindGitEmail, Value: privateEmail}))
	mustSet(t, s, p.ID, derived("from_free_email", IdentityRef{Kind: KindEmail, Value: freeEmail}))
	mustSet(t, s, p.ID, derived("from_unknown_identity", IdentityRef{Kind: KindGitHub, Value: loginB}))
	mustSet(t, s, p.ID, fact(FieldNotes, "admin note", RankAdmin))
	mustSet(t, s, p.ID, fact(FieldSlackID, "U0000TEST", RankAdmin))
	mustSet(t, s, p.ID, fact(FieldCountry, "AA", RankUser))
	local := fact(FieldLocation, "somewhere", RankUser)
	local.Locality = LocalityLocalOnly
	mustSet(t, s, p.ID, local)

	shared := exportOne(t, s, Shareable())
	gotFields := factFields(shared)
	wantFields := []string{FieldCountry, "from_free_email", "from_login"}
	if !slices.Equal(gotFields, wantFields) {
		t.Errorf("shareable facts = %v, want %v", gotFields, wantFields)
	}
	for _, i := range shared.Identities {
		if i.Value == privateEmail {
			t.Errorf("shareable export holds the private email identity")
		}
	}
	if len(shared.Consents) != 0 || len(shared.Dismissed) != 0 {
		t.Error("shareable export holds consent or dismissal bookkeeping")
	}

	// Detaching the identity a shareable fact came from makes it local.
	if err := s.RemoveIdentity(p.ID, IdentityRef{Kind: KindGitHub, Value: loginA}); err != nil {
		t.Fatalf("RemoveIdentity: %v", err)
	}
	if got := factFields(exportOne(t, s, Shareable())); slices.Contains(got, "from_login") {
		t.Errorf("a fact derived from a removed identity is still shareable: %v", got)
	}

	// Downgrading an identity to local_only later is honoured at export.
	if _, err := s.UpsertIdentity(p.ID, Identity{Kind: KindEmail, Value: freeEmail, Locality: LocalityLocalOnly}); err != nil {
		t.Fatalf("UpsertIdentity: %v", err)
	}
	if got := factFields(exportOne(t, s, Shareable())); slices.Contains(got, "from_free_email") {
		t.Errorf("a fact derived from a now local_only identity is still shareable: %v", got)
	}
}

func TestFactsFromResult(t *testing.T) {
	s := openTest(t)
	p := mustUpsert(t, s, PersonSpec{ID: "p1", Identities: []Identity{
		githubIdentity(loginA),
		{Kind: KindGitEmail, Value: privateEmail},
	}})
	now := testBase
	byLogin := resolver.Result{
		Affiliations: []resolver.Affiliation{
			{Org: "Old Org", Until: now.Add(-time.Hour), Source: "gitdm", Confidence: resolver.ConfidenceHigh, MatchedBy: "github_login"},
			{Org: "Org One", From: now.Add(-time.Hour), Source: "gitdm", Confidence: resolver.ConfidenceHigh, MatchedBy: "github_login"},
		},
		Activity: []resolver.Activity{{Project: "All  CNCF", Contributions: 7, Source: "devstats"}},
	}
	facts := FactsFromResult(byLogin, resolver.Query{GitHubLogin: "@" + loginA}, now)
	if len(facts) != 2 {
		t.Fatalf("facts = %+v, want an affiliation and an activity", facts)
	}
	aff, act := facts[0], facts[1]
	if aff.Field != FieldAffiliation || aff.Value != "Org One" || aff.Source != "colony:gitdm" || aff.Confidence != resolver.ConfidenceHigh || !aff.Suggested || aff.Rank != RankEnrichment {
		t.Errorf("affiliation fact = %+v", aff)
	}
	if act.Field != FieldActivityPrefix+"all_cncf" || act.Value != "7" || act.Source != "colony:devstats" {
		t.Errorf("activity fact = %+v", act)
	}
	for _, f := range facts {
		mustSet(t, s, p.ID, f)
	}

	byEmail := resolver.Result{Affiliations: []resolver.Affiliation{
		{Org: "Org Two", Source: "gitdm", Confidence: resolver.ConfidenceMedium, MatchedBy: "email"},
	}}
	emailFacts := FactsFromResult(byEmail, resolver.Query{GitHubLogin: loginA, Emails: []string{privateEmail}}, now)
	if len(emailFacts) != 1 || len(emailFacts[0].DerivedFrom) != 1 || emailFacts[0].DerivedFrom[0].Kind != KindEmail {
		t.Fatalf("email match facts = %+v, want one fact derived from the email only", emailFacts)
	}
	emailFact := emailFacts[0]
	emailFact.Field = "affiliation_by_email"
	mustSet(t, s, p.ID, emailFact)

	got := factFields(exportOne(t, s, Shareable()))
	if !slices.Contains(got, FieldAffiliation) || slices.Contains(got, "affiliation_by_email") {
		t.Errorf("shareable facts = %v, want the login match only", got)
	}

	unlabelled := resolver.Result{Affiliations: []resolver.Affiliation{{Org: "Org Three", Source: "other"}}}
	refs := FactsFromResult(unlabelled, resolver.Query{GitHubLogin: loginA, Emails: []string{privateEmail}}, now)[0].DerivedFrom
	if len(refs) != 2 {
		t.Errorf("an unlabelled match derived from %+v, want both identities", refs)
	}
}

func TestConsentFirst(t *testing.T) {
	s := openTest(t)
	p := mustUpsert(t, s, PersonSpec{ID: "p1", Identities: []Identity{githubIdentity(loginA)}})
	mustSet(t, s, p.ID, fact(FieldRealName, "Test Person", RankUser))
	mustSet(t, s, p.ID, fact(FieldLocation, "Test City", RankUser))
	mustSet(t, s, p.ID, fact(FieldCountry, "AA", RankUser))
	const scope = "hive-under-test"

	fields := func() []string { return factFields(exportOne(t, s, ShareableFor(scope))) }
	if got := fields(); !slices.Equal(got, []string{FieldCountry}) {
		t.Errorf("without consent: %v, want country only", got)
	}
	if err := s.SetConsent(p.ID, Consent{Scope: scope, Fields: []string{FieldRealName}, TextVersion: "v1", GrantedAt: testBase}); err != nil {
		t.Fatalf("SetConsent: %v", err)
	}
	if got := fields(); !slices.Equal(got, []string{FieldCountry, FieldRealName}) {
		t.Errorf("with real_name consent: %v", got)
	}
	if got := factFields(exportOne(t, s, ConsentFirst(scope), Shareable())); !slices.Equal(got, []string{FieldCountry, FieldRealName}) {
		t.Errorf("ConsentFirst then Shareable: %v", got)
	}
	if got := factFields(exportOne(t, s, Shareable(), ConsentFirst(scope))); !slices.Equal(got, []string{FieldCountry}) {
		t.Errorf("Shareable then ConsentFirst must fail closed: %v", got)
	}
	if got := factFields(exportOne(t, s, ShareableFor("another-hive"))); !slices.Equal(got, []string{FieldCountry}) {
		t.Errorf("a consent for one scope leaked to another: %v", got)
	}
	if err := s.MarkConsentDelivered(p.ID, scope); err != nil {
		t.Fatalf("MarkConsentDelivered: %v", err)
	}
	if c, _ := mustGet(t, s, p.ID).Consent(scope); c.DeliveredAt.IsZero() {
		t.Error("delivery was not stamped")
	}
	if err := s.WithdrawConsent(p.ID, scope); err != nil {
		t.Fatalf("WithdrawConsent: %v", err)
	}
	if got := fields(); !slices.Equal(got, []string{FieldCountry}) {
		t.Errorf("after withdrawal: %v, want country only", got)
	}
	if err := s.WithdrawConsent(p.ID, "other-scope"); !errors.Is(err, ErrNoConsent) {
		t.Errorf("withdraw unknown scope: err = %v, want ErrNoConsent", err)
	}
}

func exportOne(t *testing.T, s *Store, filters ...Filter) Person {
	t.Helper()
	snap, err := s.Export(filters...)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(snap.People) != 1 {
		t.Fatalf("export has %d people, want 1", len(snap.People))
	}
	if len(filters) > 0 && (len(snap.Tombstones) > 0 || snap.TombstoneSalt != "") {
		t.Fatal("a filtered export carries tombstones or the salt")
	}
	return snap.People[0]
}

func factFields(p Person) []string {
	var out []string
	for _, f := range p.Facts {
		out = append(out, f.Field)
	}
	slices.Sort(out)
	return out
}
