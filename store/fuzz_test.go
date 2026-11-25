package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

// FuzzImport feeds arbitrary snapshots to Import. Whatever is accepted, a
// Shareable export must never hold a local_only identity, a private-domain
// email, or a fact Shareable would refuse.
func FuzzImport(f *testing.F) {
	seed := Snapshot{SchemaVersion: SchemaVersion, People: []Person{{
		ID:     "p1",
		Status: "confirmed",
		Identities: []Identity{
			{Kind: KindGitHub, Value: loginA, Locality: LocalityShareable},
			{Kind: KindEmail, Value: privateEmail, Locality: LocalityShareable},
			{Kind: KindEmail, Value: freeEmail, Locality: LocalityShareable},
		},
		Facts: []Fact{
			{Field: FieldAffiliation, Value: "Org One", Rank: RankEnrichment, Source: "colony:gitdm", Locality: LocalityShareable, Suggested: true,
				DerivedFrom: []IdentityRef{{Kind: KindEmail, Value: privateEmail}}},
			{Field: FieldCountry, Value: "AA", Rank: RankHubShared, Source: "hub:person", Locality: LocalityShareable},
			{Field: FieldNotes, Value: "note", Rank: RankAdmin, Source: "admin", Locality: LocalityShareable},
		},
		Consents: []Consent{{Scope: "hub", Fields: []string{FieldRealName}}},
	}}}
	raw, err := json.Marshal(seed)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(raw)
	f.Add([]byte(`{"schema_version":1,"people":[{"id":"","identities":[{"kind":"git_email","value":"A@Corp.Example","locality":"shareable"}]}]}`))
	f.Add([]byte(`{"people":[{"id":"x","facts":[{"field":"real_name","rank":"user","source":"s","value":""}]}],"tombstones":["00"]}`))
	f.Add([]byte(`{}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var snap Snapshot
		if json.Unmarshal(data, &snap) != nil {
			return
		}
		s, err := Open(filepath.Join(t.TempDir(), DefaultFileName), testOptions())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close() }()
		_, _ = s.Import(context.Background(), snap)

		full, err := s.Export()
		if err != nil {
			t.Fatalf("Export after Import: %v", err)
		}
		shared, err := s.Export(Shareable())
		if err != nil {
			t.Fatalf("Shareable export: %v", err)
		}
		if len(shared.People) != len(full.People) {
			t.Fatalf("Shareable dropped people: %d of %d", len(shared.People), len(full.People))
		}
		fm := newFreeMail([]string{testFreeMail})
		for i, p := range shared.People {
			for _, id := range p.Identities {
				if id.Locality != LocalityShareable {
					t.Fatalf("shareable export holds a %q identity", id.Locality)
				}
				if id.Kind.IsEmail() && !fm.has(id.Value) {
					t.Fatalf("shareable export holds a private-domain email")
				}
			}
			for _, fa := range p.Facts {
				if !full.People[i].factShareable(fa) {
					t.Fatalf("shareable export holds fact %q that is not shareable", fa.Field)
				}
			}
			if len(p.Consents) > 0 || len(p.Dismissed) > 0 {
				t.Fatal("shareable export holds consent or dismissal bookkeeping")
			}
		}
		if len(shared.Tombstones) > 0 || shared.TombstoneSalt != "" {
			t.Fatal("shareable export holds tombstones")
		}
	})
}
