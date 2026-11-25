package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Snapshot is the portable form of a store: what Export returns and Import
// reads.
type Snapshot struct {
	SchemaVersion int       `json:"schema_version"`
	ExportedAt    time.Time `json:"exported_at,omitzero"`
	People        []Person  `json:"people"`
	// Tombstones are salted hashes of forgotten identities. They are only
	// in an unfiltered export, together with the salt they were made with.
	Tombstones    []string `json:"tombstones,omitempty"`
	TombstoneSalt string   `json:"tombstone_salt,omitempty"`
}

// Filter narrows one person in an export. It returns the narrowed person,
// and false to drop the person entirely.
type Filter func(Person) (Person, bool)

// Shareable keeps only what may leave the deployment: shareable identities,
// and shareable facts that were not derived from a local_only identity (or
// from one the person no longer holds). Consents and dismissals are local
// bookkeeping and are dropped. Every path that sends data to another
// deployment must export through this filter.
func Shareable() Filter {
	return func(p Person) (Person, bool) { return ShareableView(p), true }
}

// ShareableView applies the Shareable rules to one person.
func ShareableView(p Person) Person {
	out := Person{ID: p.ID, Status: p.Status, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt}
	for _, i := range p.Identities {
		if i.Locality == LocalityShareable {
			out.Identities = append(out.Identities, i)
		}
	}
	for _, f := range p.Facts {
		if p.factShareable(f) {
			f.DerivedFrom = slices.Clone(f.DerivedFrom)
			out.Facts = append(out.Facts, f)
		}
	}
	return out
}

// ConsentFirst drops every ConsentFirstFields fact unless the person holds
// an active consent for scope that covers the field. It reads the consents
// of the person it is given, so it must run before Shareable, which drops
// them; run after it, it drops every consent-first field (fail closed).
// ShareableFor combines the two in the right order.
func ConsentFirst(scope string) Filter {
	return func(p Person) (Person, bool) {
		c, _ := p.Consent(scope)
		p.Facts = slices.DeleteFunc(slices.Clone(p.Facts), func(f Fact) bool {
			return slices.Contains(ConsentFirstFields, f.Field) && !c.Covers(f.Field)
		})
		return p, true
	}
}

// ShareableFor is the filter for a path that sends data to the deployment
// identified by scope: ConsentFirst(scope), then Shareable.
func ShareableFor(scope string) Filter {
	consent, share := ConsentFirst(scope), Shareable()
	return func(p Person) (Person, bool) {
		p, keep := consent(p)
		if !keep {
			return p, false
		}
		return share(p)
	}
}

// Export returns every person, narrowed by filters in order. An unfiltered
// export is a full backup and carries the tombstones and their salt; a
// filtered export never does.
func (s *Store) Export(filters ...Filter) (Snapshot, error) {
	snap := Snapshot{SchemaVersion: SchemaVersion, ExportedAt: s.now().UTC(), People: []Person{}}
	err := s.db.View(func(tx *bolt.Tx) error {
		err := tx.Bucket(bucketPeople).ForEach(func(_, v []byte) error {
			var p Person
			if err := json.Unmarshal(v, &p); err != nil {
				return fmt.Errorf("store: decode person: %w", err)
			}
			for _, f := range filters {
				var keep bool
				if p, keep = f(p); !keep {
					return nil
				}
			}
			snap.People = append(snap.People, p)
			return nil
		})
		if err != nil || len(filters) > 0 {
			return err
		}
		snap.TombstoneSalt = hex.EncodeToString(tx.Bucket(bucketMeta).Get(keySalt))
		return tx.Bucket(bucketTombstones).ForEach(func(k, _ []byte) error {
			snap.Tombstones = append(snap.Tombstones, string(k))
			return nil
		})
	})
	return snap, err
}

// ImportReport counts what Import or Migrate did. It holds no values.
type ImportReport struct {
	People     int `json:"people"`
	Identities int `json:"identities"`
	Facts      int `json:"facts"`
	Consents   int `json:"consents"`
	Tombstones int `json:"tombstones"`
	// SkippedIdentities were forgotten before and were not re-added.
	SkippedIdentities int `json:"skipped_identities"`
	// SkippedFacts were stale, dismissed, or derived from a skipped
	// identity.
	SkippedFacts int `json:"skipped_facts"`
}

// Import merges a snapshot in one transaction: either every record is
// written or none is. People are merged through the same rules as Upsert
// and SetFact, so importing never lets a weaker rank overwrite a stronger
// one, and a forgotten identity is skipped rather than re-added.
func (s *Store) Import(ctx context.Context, snap Snapshot) (ImportReport, error) {
	if snap.SchemaVersion > SchemaVersion {
		return ImportReport{}, fmt.Errorf("%w: snapshot has %d, this release supports %d", ErrSchemaTooNew, snap.SchemaVersion, SchemaVersion)
	}
	var rep ImportReport
	err := s.db.Update(func(tx *bolt.Tx) error {
		if len(snap.Tombstones) > 0 {
			if err := s.importTombstones(tx, snap); err != nil {
				return err
			}
			rep.Tombstones = len(snap.Tombstones)
		}
		for _, p := range snap.People {
			if err := ctx.Err(); err != nil {
				return err
			}
			r := Record{
				Person:    PersonSpec{ID: p.ID, Status: p.Status, Identities: p.Identities},
				Facts:     p.Facts,
				Consents:  p.Consents,
				Dismissed: p.Dismissed,
				CreatedAt: p.CreatedAt,
			}
			if err := s.importRecord(tx, r, &rep); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return ImportReport{}, err
	}
	return rep, nil
}

func (s *Store) importTombstones(tx *bolt.Tx, snap Snapshot) error {
	salt, err := hex.DecodeString(snap.TombstoneSalt)
	if err != nil || len(salt) == 0 {
		return fmt.Errorf("%w: snapshot tombstones carry no valid salt", ErrSaltMismatch)
	}
	if !bytes.Equal(salt, tx.Bucket(bucketMeta).Get(keySalt)) {
		// A different salt is adopted only by a store that has nothing
		// hashed with its own yet. The rest of this transaction then
		// hashes with the adopted salt.
		if tx.Bucket(bucketTombstones).Stats().KeyN > 0 || tx.Bucket(bucketPeople).Stats().KeyN > 0 {
			return ErrSaltMismatch
		}
		if err := tx.Bucket(bucketMeta).Put(keySalt, salt); err != nil {
			return err
		}
	}
	stamp := []byte(s.now().UTC().Format(time.RFC3339))
	for _, t := range snap.Tombstones {
		if _, err := hex.DecodeString(t); err != nil || len(t) != hex.EncodedLen(sha256.Size) {
			return invalid("malformed tombstone")
		}
		if err := tx.Bucket(bucketTombstones).Put([]byte(t), stamp); err != nil {
			return err
		}
	}
	return nil
}

// Record is one person from a legacy store, as a migration Source yields it.
type Record struct {
	Person    PersonSpec  `json:"person"`
	Facts     []Fact      `json:"facts,omitempty"`
	Consents  []Consent   `json:"consents,omitempty"`
	Dismissed []Dismissal `json:"dismissed,omitempty"`
	// Forgotten lists identities the legacy store tombstoned. They are
	// tombstoned here too.
	Forgotten []IdentityRef `json:"forgotten,omitempty"`
	// CreatedAt keeps the legacy creation time of a new person.
	CreatedAt time.Time `json:"created_at,omitzero"`
}

// Source is a legacy store a consumer migrates from, for example a JSON
// people file. The consumer implements it; colony knows nothing about the
// legacy shape.
type Source interface {
	// Fingerprint identifies the source content, for example the SHA-256
	// of the file. Migrate records it.
	Fingerprint() (string, error)
	// Records calls fn once per person. It stops at the first error fn
	// returns and returns that error.
	Records(ctx context.Context, fn func(Record) error) error
}

// MigrationRecord is what Migrate stores in the meta bucket.
type MigrationRecord struct {
	Name        string       `json:"name"`
	Fingerprint string       `json:"fingerprint"`
	At          time.Time    `json:"at"`
	Report      ImportReport `json:"report"`
}

// MigrateOptions tunes Migrate.
type MigrateOptions struct {
	// Reimport imports again even when a migration under the same name
	// already ran. Records merge through the precedence rules.
	Reimport bool
}

// MigrateResult reports what Migrate did.
type MigrateResult struct {
	// Skipped is true when the migration had already run and Reimport was
	// not set. Record then describes the earlier run.
	Skipped bool
	// FingerprintChanged is true when the source differs from the one the
	// earlier run recorded.
	FingerprintChanged bool
	Record             MigrationRecord
}

// Migrate imports every record of src under name in one transaction, then
// records the source fingerprint, the time and the counts in the meta bucket.
// A migration that already ran is skipped unless opts.Reimport is set.
func (s *Store) Migrate(ctx context.Context, name string, src Source, opts MigrateOptions) (MigrateResult, error) {
	if err := checkText("migration name", name, maxFieldBytes, true); err != nil {
		return MigrateResult{}, err
	}
	fp, err := src.Fingerprint()
	if err != nil {
		return MigrateResult{}, fmt.Errorf("store: migration source fingerprint: %w", err)
	}
	prev, ran, err := s.Migration(name)
	if err != nil {
		return MigrateResult{}, err
	}
	if ran && !opts.Reimport {
		return MigrateResult{Skipped: true, FingerprintChanged: prev.Fingerprint != fp, Record: prev}, nil
	}
	var rec MigrationRecord
	err = s.db.Update(func(tx *bolt.Tx) error {
		var rep ImportReport
		if err := src.Records(ctx, func(r Record) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return s.importRecord(tx, r, &rep)
		}); err != nil {
			return err
		}
		rec = MigrationRecord{Name: name, Fingerprint: fp, At: s.now().UTC(), Report: rep}
		raw, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return tx.Bucket(bucketMeta).Put([]byte(migrationPrefix+name), raw)
	})
	if err != nil {
		return MigrateResult{}, err
	}
	return MigrateResult{FingerprintChanged: ran && prev.Fingerprint != fp, Record: rec}, nil
}

// Migration returns the record of the migration that ran under name.
func (s *Store) Migration(name string) (MigrationRecord, bool, error) {
	var rec MigrationRecord
	var ok bool
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get([]byte(migrationPrefix + name))
		if raw == nil {
			return nil
		}
		ok = true
		return json.Unmarshal(raw, &rec)
	})
	return rec, ok, err
}

func (s *Store) importRecord(tx *bolt.Tx, r Record, rep *ImportReport) error {
	for _, ref := range r.Forgotten {
		if err := checkRef(ref); err != nil {
			return err
		}
		if err := s.tombstoneTx(tx, ref); err != nil {
			return err
		}
		rep.Tombstones++
	}
	spec := r.Person
	spec.Identities = nil
	var skipped []IdentityRef
	for _, ident := range r.Person.Identities {
		if err := checkRef(ident.Ref()); err != nil {
			return err
		}
		tomb, err := s.tombstonedTx(tx, ident.Ref())
		if err != nil {
			return err
		}
		if tomb {
			skipped = append(skipped, ident.Ref())
			rep.SkippedIdentities++
			continue
		}
		spec.Identities = append(spec.Identities, ident)
	}
	if spec.ID == "" && len(spec.Identities) == 0 {
		// Nothing left to recognise this person by.
		rep.SkippedFacts += len(r.Facts)
		return nil
	}
	p, err := s.upsertTx(tx, spec)
	if err != nil {
		return err
	}
	rep.People++
	rep.Identities += len(spec.Identities)
	if !r.CreatedAt.IsZero() && (p.CreatedAt.IsZero() || r.CreatedAt.Before(p.CreatedAt)) {
		p.CreatedAt = r.CreatedAt.UTC()
	}
	for _, d := range r.Dismissed {
		if err := checkText("dismissed field", d.Field, maxFieldBytes, true); err != nil {
			return err
		}
		if err := checkText("dismissed value", d.Value, maxValueBytes, true); err != nil {
			return err
		}
		if err := checkText("dismissed by", d.By, maxByBytes, false); err != nil {
			return err
		}
		if !p.dismissed(d.Field, d.Value) {
			d.At = d.At.UTC()
			p.Dismissed = append(p.Dismissed, d)
		}
	}
	for _, f := range r.Facts {
		if slices.ContainsFunc(f.DerivedFrom, func(d IdentityRef) bool { return slices.ContainsFunc(skipped, sameIdentity(d)) }) {
			rep.SkippedFacts++
			continue
		}
		res, err := s.setFact(&p, f)
		if err != nil {
			return err
		}
		if res.Stored {
			rep.Facts++
		} else {
			rep.SkippedFacts++
		}
	}
	for _, c := range r.Consents {
		if err := checkConsent(c); err != nil {
			return err
		}
		setConsent(&p, c)
		rep.Consents++
	}
	return putPerson(tx, p)
}

func sameIdentity(a IdentityRef) func(IdentityRef) bool {
	return func(b IdentityRef) bool {
		return identityClass(a.Kind) == identityClass(b.Kind) && normalizeValue(a.Kind, a.Value) == normalizeValue(b.Kind, b.Value)
	}
}
