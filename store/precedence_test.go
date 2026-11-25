package store

import (
	"errors"
	"testing"
	"time"
)

func TestRankOrder(t *testing.T) {
	ranks := Ranks()
	for i := range ranks {
		for j := range ranks {
			if got, want := ranks[i].Outranks(ranks[j]), i < j; got != want {
				t.Errorf("%s.Outranks(%s) = %v, want %v", ranks[i], ranks[j], got, want)
			}
		}
	}
	if Rank("bogus").Outranks(RankEnrichment) {
		t.Error("an unknown rank must not outrank enrichment")
	}
}

type write struct {
	rank  Rank
	value string
	at    time.Duration // offset from testBase
}

func TestPrecedenceTable(t *testing.T) {
	cases := []struct {
		name      string
		writes    []write
		wantValue string
		wantRank  Rank
	}{
		{"enrichment alone", []write{{RankEnrichment, "Org E", 0}}, "Org E", RankEnrichment},
		{"user beats later enrichment", []write{{RankUser, "Org U", 0}, {RankEnrichment, "Org E", time.Hour}}, "Org U", RankUser},
		{"user beats earlier enrichment", []write{{RankEnrichment, "Org E", 0}, {RankUser, "Org U", time.Hour}}, "Org U", RankUser},
		{"admin beats hub", []write{{RankAdmin, "Org A", 0}, {RankHubShared, "Org H", time.Hour}}, "Org A", RankAdmin},
		{"hub beats enrichment", []write{{RankEnrichment, "Org E", time.Hour}, {RankHubShared, "Org H", 0}}, "Org H", RankHubShared},
		{"user beats admin", []write{{RankAdmin, "Org A", time.Hour}, {RankUser, "Org U", 0}}, "Org U", RankUser},
		{"user clear hides admin", []write{{RankUser, "", 0}, {RankAdmin, "Org A", time.Hour}}, "", RankUser},
		{"admin clear hides enrichment", []write{{RankEnrichment, "Org E", 0}, {RankAdmin, "", 0}}, "", RankAdmin},
		{"same rank newer wins", []write{{RankAdmin, "Old", 0}, {RankAdmin, "New", time.Hour}}, "New", RankAdmin},
		{"same rank older loses", []write{{RankAdmin, "New", time.Hour}, {RankAdmin, "Old", 0}}, "New", RankAdmin},
		{"same rank same instant last wins", []write{{RankUser, "First", 0}, {RankUser, "Second", 0}}, "Second", RankUser},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openTest(t)
			p := mustUpsert(t, s, PersonSpec{ID: "p1", Identities: []Identity{githubIdentity(loginA)}})
			for _, w := range tc.writes {
				f := fact(FieldOrganization, w.value, w.rank)
				f.At = testBase.Add(w.at)
				mustSet(t, s, p.ID, f)
			}
			got, ok := mustGet(t, s, p.ID).Effective(FieldOrganization)
			if !ok {
				t.Fatal("no effective value")
			}
			if got.Value != tc.wantValue || got.Rank != tc.wantRank {
				t.Errorf("effective = %q at %s, want %q at %s", got.Value, got.Rank, tc.wantValue, tc.wantRank)
			}
		})
	}
}

func TestEnrichmentNeverOverwritesStrongerRank(t *testing.T) {
	s := openTest(t)
	p := mustUpsert(t, s, PersonSpec{ID: "p1"})
	for _, strong := range []Rank{RankUser, RankAdmin, RankHubShared} {
		field := "field_" + string(strong)
		mustSet(t, s, p.ID, fact(field, "kept", strong))
		res := mustSet(t, s, p.ID, Fact{
			Field: field, Value: "suggested", Rank: RankEnrichment, Source: "colony:gitdm",
			At: testBase.Add(time.Hour), Suggested: true,
		})
		if !res.Stored || res.Effective {
			t.Errorf("%s: SetFact = %+v, want stored but not effective", strong, res)
		}
		got, _ := mustGet(t, s, p.ID).Effective(field)
		if got.Value != "kept" || got.Rank != strong {
			t.Errorf("%s: effective = %q at %s, want the %s value", strong, got.Value, got.Rank, strong)
		}
		if kept, ok := mustGet(t, s, p.ID).FactAt(field, strong); !ok || kept.Value != "kept" {
			t.Errorf("%s: the stronger record changed: %+v", strong, kept)
		}
	}
}

func TestSetFactStaleReason(t *testing.T) {
	s := openTest(t)
	p := mustUpsert(t, s, PersonSpec{ID: "p1"})
	newer := fact(FieldCountry, "AA", RankUser)
	newer.At = testBase.Add(time.Hour)
	mustSet(t, s, p.ID, newer)
	res := mustSet(t, s, p.ID, fact(FieldCountry, "BB", RankUser))
	if res.Stored || res.Reason != ReasonStale {
		t.Errorf("older write = %+v, want ReasonStale", res)
	}
}

func TestSetFactValidation(t *testing.T) {
	s := openTest(t)
	p := mustUpsert(t, s, PersonSpec{ID: "p1"})
	bad := map[string]Fact{
		"unknown rank":         {Field: FieldCountry, Value: "AA", Rank: "boss", Source: "test"},
		"no source":            {Field: FieldCountry, Value: "AA", Rank: RankUser},
		"no field":             {Value: "AA", Rank: RankUser, Source: "test"},
		"hub without label":    {Field: FieldCountry, Value: "AA", Rank: RankHubShared, Source: "person"},
		"hub bare prefix":      {Field: FieldCountry, Value: "AA", Rank: RankHubShared, Source: HubSourcePrefix},
		"suggested user value": {Field: FieldCountry, Value: "AA", Rank: RankUser, Source: "test", Suggested: true},
		"unknown confidence":   {Field: FieldCountry, Value: "AA", Rank: RankUser, Source: "test", Confidence: "certain"},
		"unknown locality":     {Field: FieldCountry, Value: "AA", Rank: RankUser, Source: "test", Locality: "public"},
		"until before from":    {Field: FieldCountry, Value: "AA", Rank: RankUser, Source: "test", From: testBase, Until: testBase.Add(-time.Hour)},
		"control character":    {Field: FieldCountry, Value: "A\x00A", Rank: RankUser, Source: "test"},
		"bad derived identity": {Field: FieldCountry, Value: "AA", Rank: RankUser, Source: "test", DerivedFrom: []IdentityRef{{Kind: "fax", Value: "1"}}},
	}
	for name, f := range bad {
		if _, err := s.SetFact(p.ID, f); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := s.SetFact("missing", fact(FieldCountry, "AA", RankUser)); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown person: err = %v, want ErrNotFound", err)
	}
	if len(mustGet(t, s, p.ID).Facts) != 0 {
		t.Error("a rejected fact was stored")
	}
}

func TestDeleteFactRevealsWeakerRank(t *testing.T) {
	s := openTest(t)
	p := mustUpsert(t, s, PersonSpec{ID: "p1"})
	mustSet(t, s, p.ID, fact(FieldCountry, "AA", RankEnrichment))
	mustSet(t, s, p.ID, fact(FieldCountry, "BB", RankAdmin))
	removed, err := s.DeleteFact(p.ID, FieldCountry, RankAdmin)
	if err != nil || !removed {
		t.Fatalf("DeleteFact = %v, %v", removed, err)
	}
	got, _ := mustGet(t, s, p.ID).Effective(FieldCountry)
	if got.Value != "AA" || got.Rank != RankEnrichment {
		t.Errorf("effective = %q at %s, want the enrichment value", got.Value, got.Rank)
	}
	if removed, err := s.DeleteFact(p.ID, FieldCountry, RankAdmin); err != nil || removed {
		t.Errorf("second DeleteFact = %v, %v, want false, nil", removed, err)
	}
}

func TestSuggestionLifecycle(t *testing.T) {
	s := openTest(t)
	p := mustUpsert(t, s, PersonSpec{ID: "p1", Identities: []Identity{githubIdentity(loginA)}})
	suggest := func(value string, at time.Duration) SetResult {
		return mustSet(t, s, p.ID, Fact{
			Field: FieldAffiliation, Value: value, Rank: RankEnrichment, Source: "colony:gitdm",
			Confidence: "high", Locality: LocalityShareable, At: testBase.Add(at), Suggested: true,
		})
	}
	suggest("Org One", 0)

	view := mustGet(t, s, p.ID).EffectiveView(ViewOptions{ExcludeSuggested: true})
	if _, ok := view[FieldAffiliation]; ok {
		t.Error("ExcludeSuggested view shows an unaccepted suggestion")
	}
	if f, ok := mustGet(t, s, p.ID).Effective(FieldAffiliation); !ok || !f.Suggested {
		t.Errorf("default view = %+v, %v, want the suggestion flagged", f, ok)
	}

	accepted, err := s.AcceptSuggestion(p.ID, FieldAffiliation, "owner")
	if err != nil {
		t.Fatalf("AcceptSuggestion: %v", err)
	}
	if accepted.Suggested || accepted.Rank != RankEnrichment || accepted.Source != "colony:gitdm" || accepted.AcceptedBy != "owner" {
		t.Errorf("accepted = %+v, want an accepted enrichment fact with its source", accepted)
	}
	suggest("Org One", time.Hour)
	if f, _ := mustGet(t, s, p.ID).Effective(FieldAffiliation); f.Suggested {
		t.Error("re-suggesting an accepted value reverted the acceptance")
	}
	if res := suggest("Org Other", time.Hour); res.Stored || res.Reason != ReasonAccepted {
		t.Errorf("a different suggestion over an accepted value = %+v, want ReasonAccepted", res)
	}
	if f, _ := mustGet(t, s, p.ID).Effective(FieldAffiliation); f.Value != "Org One" {
		t.Errorf("accepted value replaced by %q", f.Value)
	}

	if err := s.DismissSuggestion(p.ID, FieldAffiliation, "owner"); err != nil {
		t.Fatalf("DismissSuggestion: %v", err)
	}
	if _, ok := mustGet(t, s, p.ID).Effective(FieldAffiliation); ok {
		t.Error("a dismissed value is still effective")
	}
	if res := suggest("  org   ONE ", 2*time.Hour); res.Stored || res.Reason != ReasonDismissed {
		t.Errorf("re-suggesting a dismissed value = %+v, want ReasonDismissed", res)
	}
	if res := suggest("Org Two", 2*time.Hour); !res.Stored {
		t.Errorf("a different value = %+v, want stored", res)
	}
	if err := s.DismissSuggestion(p.ID, FieldCountry, "owner"); !errors.Is(err, ErrNoSuggestion) {
		t.Errorf("dismiss with nothing to dismiss: err = %v, want ErrNoSuggestion", err)
	}
	if _, err := s.AcceptSuggestion(p.ID, FieldCountry, "owner"); !errors.Is(err, ErrNoSuggestion) {
		t.Errorf("accept with nothing to accept: err = %v, want ErrNoSuggestion", err)
	}
}

func TestEffectiveView(t *testing.T) {
	s := openTest(t)
	p := mustUpsert(t, s, PersonSpec{ID: "p1"})
	mustSet(t, s, p.ID, fact(FieldCountry, "AA", RankEnrichment))
	mustSet(t, s, p.ID, fact(FieldCountry, "BB", RankUser))
	mustSet(t, s, p.ID, fact(FieldOrganization, "Org H", RankHubShared))
	view := mustGet(t, s, p.ID).EffectiveView(ViewOptions{})
	if len(view) != 2 || view[FieldCountry].Value != "BB" || view[FieldOrganization].Value != "Org H" {
		t.Errorf("view = %+v", view)
	}
	if view[FieldOrganization].Source != HubSourcePrefix+"test" {
		t.Errorf("hub value lost its label: %+v", view[FieldOrganization])
	}
}
