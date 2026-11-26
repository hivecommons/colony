// Package store is colony's embedded customer-data store: people, their
// identities, facts with per-field provenance (rank, source, confidence,
// locality), consent records and tombstones, in one bbolt file per
// deployment.
//
// The store is a library, not a service. It opens no listener and sends
// nothing anywhere. Each deployment (a hub, a spoke) opens its own file, and
// bbolt's file lock keeps it to one process at a time.
//
// Precedence: every field holds at most one fact per rank, and the effective
// value is the one at the strongest rank present (user > admin > hub_shared >
// enrichment). A write at one rank never touches another rank, so an
// enrichment value can never overwrite a value a person or an administrator
// set. At the same rank the newer value wins. An empty value is an explicit
// clear at its rank.
//
// Locality: an identity or fact is shareable or local_only, and the empty
// locality is stored as local_only. Every email on a domain that is not
// free-mail is local_only, and so is every fact derived from a local_only
// identity. Export with the Shareable filter is the only export a path that
// sends data to another deployment may use.
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/colony/resolver"
	bolt "go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

// SchemaVersion is the schema this release writes. Open upgrades an older
// file and refuses a newer one.
//
// Version 2 (v0.4.0) adds identity provenance (Manual, By, Score), held
// identities and their index, provider answers, attachments, and the
// consent delivery bookkeeping. Every new field is optional, so a version 1
// record reads unchanged.
const SchemaVersion = 2

// DefaultLockTimeout is how long Open waits for another process to release
// the file lock.
const DefaultLockTimeout = 5 * time.Second

// MaxClockSkew is how far in the future a fact's At may be before SetFact
// clamps it to now.
const MaxClockSkew = 5 * time.Minute

// DefaultFileName is the conventional file name inside a deployment's data
// directory.
const DefaultFileName = "colony.db"

// fileMode keeps the store readable by its owner only.
const fileMode os.FileMode = 0o600

const (
	saltBytes        = 32
	generatedIDBytes = 16
)

var (
	bucketMeta       = []byte("meta")
	bucketPeople     = []byte("people")
	bucketIdentities = []byte("identities")
	bucketTombstones = []byte("tombstones")
	// bucketHeld indexes held identities: identity key to the ids of the
	// people holding it.
	bucketHeld = []byte("held")

	keySchemaVersion = []byte("schema_version")
	keySalt          = []byte("tombstone_salt")
	migrationPrefix  = "migration:"
	// attachmentPrefix keys store-level attachments in the meta bucket.
	attachmentPrefix = "attachment:"

	// personTombstoneClass is not an identity kind, so a person id can never
	// collide with an identity tombstone.
	personTombstoneClass = "person"
)

// ErrLocked is returned by Open when another process holds the file.
var ErrLocked = errors.New("store: file is locked by another process")

// ErrNoSuggestion is returned when a field has no enrichment value to accept
// or dismiss.
var ErrNoSuggestion = errors.New("store: no enrichment value for field")

// ErrNoConsent is returned when a person has no consent record for a scope.
var ErrNoConsent = errors.New("store: no consent record for scope")

// upgrades[i] moves a file from schema version i to i+1. Every upgrade runs
// in the same transaction as the version bump, so a crash leaves the file
// at the old version.
var upgrades = []func(tx *bolt.Tx) error{
	upgradeV0toV1,
	upgradeV1toV2,
}

func upgradeV0toV1(tx *bolt.Tx) error {
	for _, b := range [][]byte{bucketMeta, bucketPeople, bucketIdentities, bucketTombstones} {
		if _, err := tx.CreateBucketIfNotExists(b); err != nil {
			return err
		}
	}
	return nil
}

// upgradeV1toV2 adds the held-identity index. Version 1 records hold no
// held identity, so the index starts empty; every other version 2 field is
// optional in the record itself.
func upgradeV1toV2(tx *bolt.Tx) error {
	_, err := tx.CreateBucketIfNotExists(bucketHeld)
	return err
}

// Options configures Open.
type Options struct {
	// LockTimeout bounds the wait for the file lock. Zero means
	// DefaultLockTimeout.
	LockTimeout time.Duration
	// FreeMailDomains replaces DefaultFreeMailDomains when non-nil.
	FreeMailDomains []string
	// Now replaces time.Now, for tests.
	Now func() time.Time
}

// Store is an open colony store. It is safe for concurrent use.
type Store struct {
	db       *bolt.DB
	now      func() time.Time
	freeMail freeMail
}

// Open opens or creates the store file at path. A file written by a newer
// colony release fails with ErrSchemaTooNew and is left untouched.
func Open(path string, opts Options) (*Store, error) {
	timeout := opts.LockTimeout
	if timeout <= 0 {
		timeout = DefaultLockTimeout
	}
	db, err := bolt.Open(path, fileMode, &bolt.Options{Timeout: timeout})
	if err != nil {
		if errors.Is(err, bolterrors.ErrTimeout) {
			return nil, fmt.Errorf("%w: %s", ErrLocked, path)
		}
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	s := &Store{db: db, now: opts.Now, freeMail: newFreeMail(opts.FreeMailDomains)}
	if s.now == nil {
		s.now = time.Now
	}
	if err := s.initialize(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) initialize() error {
	var version int
	if err := s.db.View(func(tx *bolt.Tx) error {
		v, err := readSchemaVersion(tx)
		version = v
		return err
	}); err != nil {
		return err
	}
	if version > SchemaVersion {
		return fmt.Errorf("%w: file has %d, this release supports %d", ErrSchemaTooNew, version, SchemaVersion)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		for v := version; v < SchemaVersion; v++ {
			if err := upgrades[v](tx); err != nil {
				return fmt.Errorf("store: upgrade schema %d to %d: %w", v, v+1, err)
			}
			if err := tx.Bucket(bucketMeta).Put(keySchemaVersion, []byte(strconv.Itoa(v+1))); err != nil {
				return err
			}
		}
		meta := tx.Bucket(bucketMeta)
		if len(meta.Get(keySalt)) > 0 {
			return nil
		}
		salt := make([]byte, saltBytes)
		if _, err := rand.Read(salt); err != nil {
			return err
		}
		return meta.Put(keySalt, salt)
	})
}

func readSchemaVersion(tx *bolt.Tx) (int, error) {
	meta := tx.Bucket(bucketMeta)
	if meta == nil {
		return 0, nil
	}
	raw := meta.Get(keySchemaVersion)
	if raw == nil {
		return 0, nil
	}
	v, err := strconv.Atoi(string(raw))
	if err != nil || v < 0 {
		return 0, fmt.Errorf("store: unreadable schema_version %q", raw)
	}
	return v, nil
}

// Close releases the file.
func (s *Store) Close() error { return s.db.Close() }

// Path returns the file path.
func (s *Store) Path() string { return s.db.Path() }

// SchemaVersion returns the schema version recorded in the file.
func (s *Store) SchemaVersion() (int, error) {
	var v int
	err := s.db.View(func(tx *bolt.Tx) error {
		var err error
		v, err = readSchemaVersion(tx)
		return err
	})
	return v, err
}

// Get returns the person with id, or ErrNotFound.
func (s *Store) Get(id string) (Person, error) {
	var p Person
	err := s.db.View(func(tx *bolt.Tx) error {
		var ok bool
		var err error
		p, ok, err = loadPerson(tx, id)
		if err == nil && !ok {
			err = ErrNotFound
		}
		return err
	})
	return p, err
}

// FindByIdentity returns the person holding the identity, or ErrNotFound.
// Email kinds match each other.
func (s *Store) FindByIdentity(kind Kind, value string) (Person, error) {
	ref := IdentityRef{Kind: kind, Value: value}
	if err := checkRef(ref); err != nil {
		return Person{}, err
	}
	var p Person
	err := s.db.View(func(tx *bolt.Tx) error {
		id := tx.Bucket(bucketIdentities).Get(indexKey(ref))
		if id == nil {
			return ErrNotFound
		}
		var ok bool
		var err error
		p, ok, err = loadPerson(tx, string(id))
		if err == nil && !ok {
			err = ErrNotFound
		}
		return err
	})
	return p, err
}

// List calls fn for every person in id order. fn must not call back into the
// store.
func (s *Store) List(fn func(Person) error) error {
	return s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketPeople).ForEach(func(_, v []byte) error {
			var p Person
			if err := json.Unmarshal(v, &p); err != nil {
				return fmt.Errorf("store: decode person: %w", err)
			}
			return fn(p)
		})
	})
}

// PersonSpec is the input to Upsert.
type PersonSpec struct {
	// ID is the stable person id. The caller may supply it, so ids from an
	// older store survive migration. When empty, Upsert reuses the person
	// that already holds one of the identities, or generates a new id.
	ID string `json:"id,omitempty"`
	// Status replaces the stored status when non-empty. A new person
	// without one gets StatusActive.
	Status     string     `json:"status,omitempty"`
	Identities []Identity `json:"identities,omitempty"`
	// Held are identities the person claims without owning them: they are
	// kept as HeldIdentity, whoever owns them (see Person.Held).
	Held []Identity `json:"held,omitempty"`
	// HoldConflicts makes an identity in Identities that another person
	// owns a held identity, instead of failing the upsert with
	// ErrIdentityConflict.
	HoldConflicts bool `json:"hold_conflicts,omitempty"`
}

// Upsert creates the person or adds identities to it. An identity that
// another person owns fails with ErrIdentityConflict, unless
// spec.HoldConflicts is set, and a forgotten one with ErrTombstoned; either
// way nothing is written. Without an id, the person is the owner of the
// first identity someone owns (so with HoldConflicts the rest are held
// there); a claimant should pass its own id.
func (s *Store) Upsert(spec PersonSpec) (Person, error) {
	var p Person
	err := s.db.Update(func(tx *bolt.Tx) error {
		var err error
		p, err = s.upsertTx(tx, spec)
		return err
	})
	return p, err
}

// UpsertIdentity adds one identity to an existing person, or updates its
// metadata.
func (s *Store) UpsertIdentity(id string, ident Identity) (Person, error) {
	var p Person
	err := s.db.Update(func(tx *bolt.Tx) error {
		if _, ok, err := loadPerson(tx, id); err != nil {
			return err
		} else if !ok {
			return ErrNotFound
		}
		var err error
		p, err = s.upsertTx(tx, PersonSpec{ID: id, Identities: []Identity{ident}})
		return err
	})
	return p, err
}

func (s *Store) upsertTx(tx *bolt.Tx, spec PersonSpec) (Person, error) {
	now := s.now().UTC()
	idents := make([]Identity, 0, len(spec.Identities))
	explicit := make([]bool, 0, len(spec.Identities))
	for _, in := range spec.Identities {
		ident, err := s.prepareIdentity(in, now)
		if err != nil {
			return Person{}, err
		}
		idents = append(idents, ident)
		// A private-domain email is local_only however it is upserted.
		explicit = append(explicit, in.Locality != "" || s.forcedLocal(in.Kind, ident.Value))
	}
	if len(spec.Held) > maxHeld {
		return Person{}, invalid("more than %d held identities", maxHeld)
	}
	held := make([]Identity, 0, len(spec.Held))
	for _, in := range spec.Held {
		ident, err := s.prepareIdentity(in, now)
		if err != nil {
			return Person{}, err
		}
		held = append(held, ident)
	}
	if spec.Status != "" {
		if err := checkText("status", spec.Status, maxStatusBytes, false); err != nil {
			return Person{}, err
		}
	}
	index := tx.Bucket(bucketIdentities)
	for _, ident := range slices.Concat(idents, held) {
		tomb, err := s.tombstonedTx(tx, ident.Ref())
		if err != nil {
			return Person{}, err
		}
		if tomb {
			return Person{}, fmt.Errorf("%w: %s", ErrTombstoned, ident.Kind)
		}
	}
	id := spec.ID
	if id == "" {
		for _, ident := range idents {
			if owner := index.Get(indexKey(ident.Ref())); owner != nil {
				id = string(owner)
				break
			}
		}
	}
	// An identity another person owns fails the upsert, or with
	// HoldConflicts is held instead.
	var owned []Identity
	var ownedExplicit []bool
	for i, ident := range idents {
		owner := index.Get(indexKey(ident.Ref()))
		if owner != nil && string(owner) != id {
			if !spec.HoldConflicts {
				return Person{}, fmt.Errorf("%w: %s", ErrIdentityConflict, ident.Kind)
			}
			held = append(held, ident)
			continue
		}
		owned = append(owned, ident)
		ownedExplicit = append(ownedExplicit, explicit[i])
	}
	if id == "" {
		var err error
		if id, err = newID(); err != nil {
			return Person{}, err
		}
	}
	if err := checkID(id); err != nil {
		return Person{}, err
	}
	if tx.Bucket(bucketTombstones).Get(personTombstoneKey(tx, id)) != nil {
		return Person{}, fmt.Errorf("%w: person id", ErrTombstoned)
	}
	p, ok, err := loadPerson(tx, id)
	if err != nil {
		return Person{}, err
	}
	if !ok {
		p = Person{ID: id, Status: StatusActive, CreatedAt: now}
	}
	if spec.Status != "" {
		p.Status = spec.Status
	}
	for i, ident := range owned {
		mergeIdentity(&p, ident, ownedExplicit[i])
		if err := index.Put(indexKey(ident.Ref()), []byte(id)); err != nil {
			return Person{}, err
		}
		// An identity the person now owns is no longer only held.
		if err := dropHeldTx(tx, &p, ident.Ref()); err != nil {
			return Person{}, err
		}
	}
	for _, ident := range held {
		if p.HasIdentity(ident.Ref()) {
			continue
		}
		owner := string(index.Get(indexKey(ident.Ref())))
		if err := holdTx(tx, &p, HeldIdentity{Identity: ident, Owner: owner}); err != nil {
			return Person{}, err
		}
	}
	if len(p.Held) > maxHeld {
		return Person{}, invalid("more than %d held identities", maxHeld)
	}
	p.UpdatedAt = now
	return p, putPerson(tx, p)
}

// mergeIdentity adds ident or updates the stored copy. A verified identity
// stays verified, and an update that names no locality keeps the stored one
// (prepareIdentity has already forced private-domain email to local_only).
func mergeIdentity(p *Person, ident Identity, explicitLocality bool) {
	same := sameIdentity(ident.Ref())
	for i, have := range p.Identities {
		if !same(have.Ref()) {
			continue
		}
		// The first kind recorded for an address is kept.
		ident.Kind = have.Kind
		ident.Verified = ident.Verified || have.Verified
		ident.Manual = ident.Manual || have.Manual
		if !explicitLocality {
			ident.Locality = have.Locality
		}
		if ident.Source == "" {
			ident.Source = have.Source
		}
		if ident.By == "" {
			ident.By = have.By
		}
		if ident.Score == 0 {
			ident.Score = have.Score
		}
		p.Identities[i] = ident
		return
	}
	p.Identities = append(p.Identities, ident)
}

func (s *Store) prepareIdentity(in Identity, now time.Time) (Identity, error) {
	if err := checkRef(in.Ref()); err != nil {
		return Identity{}, err
	}
	if !in.Locality.Valid() {
		return Identity{}, invalid("unknown locality %q", in.Locality)
	}
	if err := checkText("identity source", in.Source, maxSourceBytes, false); err != nil {
		return Identity{}, err
	}
	if err := checkText("identity by", in.By, maxByBytes, false); err != nil {
		return Identity{}, err
	}
	if math.IsNaN(in.Score) || in.Score < 0 || in.Score > identityScoreMax {
		return Identity{}, invalid("identity score %v is outside [0, 1]", in.Score)
	}
	out := in
	out.Value = normalizeValue(in.Kind, in.Value)
	out.Locality = in.Locality.orLocal()
	if s.forcedLocal(in.Kind, out.Value) {
		out.Locality = LocalityLocalOnly
	}
	if out.At.IsZero() {
		out.At = now
	}
	out.At = out.At.UTC()
	return out, nil
}

// forcedLocal reports whether an identity is local_only whatever the caller
// asks: a private-domain email, a Slack id or an OIDC subject.
func (s *Store) forcedLocal(k Kind, value string) bool {
	return k.IsAlwaysLocal() || (k.IsEmail() && !s.freeMail.has(value))
}

// RemoveIdentity detaches an identity from a person without forgetting it.
// Facts derived from it become local_only.
func (s *Store) RemoveIdentity(id string, ref IdentityRef) error {
	if err := checkRef(ref); err != nil {
		return err
	}
	return s.update(id, func(tx *bolt.Tx, p *Person) (bool, error) {
		if !removeIdentity(p, ref) {
			return false, nil
		}
		return true, tx.Bucket(bucketIdentities).Delete(indexKey(ref))
	})
}

func removeIdentity(p *Person, ref IdentityRef) bool {
	cls, v := identityClass(ref.Kind), normalizeValue(ref.Kind, ref.Value)
	before := len(p.Identities)
	p.Identities = slices.DeleteFunc(p.Identities, func(i Identity) bool {
		return identityClass(i.Kind) == cls && normalizeValue(i.Kind, i.Value) == v
	})
	return len(p.Identities) != before
}

// Reason says why SetFact did not store a fact.
type Reason string

const (
	// ReasonStale: the same rank holds a newer value.
	ReasonStale Reason = "stale"
	// ReasonDismissed: the enrichment value was dismissed before.
	ReasonDismissed Reason = "dismissed"
	// ReasonAccepted: a suggestion did not replace an accepted value.
	ReasonAccepted Reason = "accepted"
)

// SetResult reports what SetFact did.
type SetResult struct {
	// Stored is true when the fact replaced or added the record at its rank.
	Stored bool
	// Effective is true when the fact is now the field's effective value.
	// A stored fact under a stronger rank is kept but not effective.
	Effective bool
	// Reason is set when Stored is false.
	Reason Reason
}

// SetFact stores f at its rank on person id. It never changes another rank:
// a weaker rank never overwrites a stronger one, and at the same rank the
// newer At wins (a zero At means now).
func (s *Store) SetFact(id string, f Fact) (SetResult, error) {
	var res SetResult
	err := s.update(id, func(_ *bolt.Tx, p *Person) (bool, error) {
		var err error
		res, err = s.setFact(p, f)
		return res.Stored, err
	})
	return res, err
}

func (s *Store) setFact(p *Person, in Fact) (SetResult, error) {
	f, err := s.prepareFact(*p, in)
	if err != nil {
		return SetResult{}, err
	}
	if f.Rank == RankEnrichment && f.Value != "" && p.dismissed(f.Field, f.Value) {
		return SetResult{Reason: ReasonDismissed}, nil
	}
	i := p.factIndex(f.Field, f.Rank)
	if i >= 0 {
		have := p.Facts[i]
		if f.At.Before(have.At) {
			return SetResult{Reason: ReasonStale}, nil
		}
		if f.Suggested && !have.Suggested && !have.Cleared() {
			if normalizeFactValue(f.Value) != normalizeFactValue(have.Value) {
				// A new suggestion never replaces a value someone
				// accepted; the owner dismisses it first.
				return SetResult{Reason: ReasonAccepted}, nil
			}
			// A provider re-suggesting the accepted value keeps the
			// acceptance.
			f.Suggested, f.AcceptedAt, f.AcceptedBy = false, have.AcceptedAt, have.AcceptedBy
		}
		p.Facts[i] = f
	} else {
		p.Facts = append(p.Facts, f)
	}
	eff, _ := p.Effective(f.Field)
	return SetResult{Stored: true, Effective: eff.Rank == f.Rank}, nil
}

func (s *Store) prepareFact(p Person, in Fact) (Fact, error) {
	if err := checkText("fact field", in.Field, maxFieldBytes, true); err != nil {
		return Fact{}, err
	}
	if !in.Rank.Valid() {
		return Fact{}, invalid("unknown rank %q", in.Rank)
	}
	if err := checkText("fact source", in.Source, maxSourceBytes, true); err != nil {
		return Fact{}, err
	}
	if in.Rank == RankHubShared && (!strings.HasPrefix(in.Source, HubSourcePrefix) || len(in.Source) == len(HubSourcePrefix)) {
		return Fact{}, invalid("a hub_shared fact needs a %q source label", HubSourcePrefix)
	}
	switch in.Confidence {
	case "", resolver.ConfidenceHigh, resolver.ConfidenceMedium, resolver.ConfidenceLow:
	default:
		return Fact{}, invalid("unknown confidence %q", in.Confidence)
	}
	if !in.Locality.Valid() {
		return Fact{}, invalid("unknown locality %q", in.Locality)
	}
	if in.Suggested && in.Rank != RankEnrichment {
		return Fact{}, invalid("only an enrichment fact can be a suggestion")
	}
	if err := CheckFactValue(in.Field, in.Value); err != nil {
		return Fact{}, err
	}
	if err := checkText("fact by", in.By, maxByBytes, false); err != nil {
		return Fact{}, err
	}
	if !in.From.IsZero() && !in.Until.IsZero() && in.Until.Before(in.From) {
		return Fact{}, invalid("fact until is before from")
	}
	f := in
	f.DerivedFrom = nil
	for _, ref := range in.DerivedFrom {
		if err := checkRef(ref); err != nil {
			return Fact{}, err
		}
		f.DerivedFrom = append(f.DerivedFrom, IdentityRef{Kind: ref.Kind, Value: normalizeValue(ref.Kind, ref.Value)})
	}
	now := s.now().UTC()
	if f.At.IsZero() || f.At.After(now.Add(MaxClockSkew)) {
		// A timestamp from the future would lock the rank against every
		// later write, so it is clamped to now.
		f.At = now
	}
	f.At = f.At.UTC()
	if f.Suggested {
		f.AcceptedAt, f.AcceptedBy = time.Time{}, ""
	}
	f.Locality = in.Locality.orLocal()
	if slices.Contains(alwaysLocalFields, f.Field) {
		f.Locality = LocalityLocalOnly
	}
	for _, ref := range f.DerivedFrom {
		if ref.Kind.IsEmail() && !s.freeMail.has(ref.Value) {
			f.Locality = LocalityLocalOnly
		}
		if id, ok := p.identity(ref); !ok || id.Locality != LocalityShareable {
			f.Locality = LocalityLocalOnly
		}
	}
	return f, nil
}

// DeleteFact removes the record at one rank, so the next rank down becomes
// effective. Use a fact with an empty Value instead to clear the field.
func (s *Store) DeleteFact(id, field string, rank Rank) (bool, error) {
	var removed bool
	err := s.update(id, func(_ *bolt.Tx, p *Person) (bool, error) {
		i := p.factIndex(field, rank)
		if i < 0 {
			return false, nil
		}
		p.Facts = slices.Delete(p.Facts, i, i+1)
		removed = true
		return true, nil
	})
	return removed, err
}

// AcceptSuggestion marks the enrichment value of field as accepted by by. It
// stays at rank enrichment with its source, so a stronger rank still wins
// and the owner can still dismiss it.
func (s *Store) AcceptSuggestion(id, field, by string) (Fact, error) {
	if err := checkText("accepted by", by, maxByBytes, false); err != nil {
		return Fact{}, err
	}
	var out Fact
	err := s.update(id, func(_ *bolt.Tx, p *Person) (bool, error) {
		i := p.factIndex(field, RankEnrichment)
		if i < 0 || p.Facts[i].Cleared() {
			return false, ErrNoSuggestion
		}
		f := &p.Facts[i]
		if f.Suggested {
			f.Suggested = false
			f.AcceptedAt = s.now().UTC()
			f.AcceptedBy = by
		}
		out = *f
		return true, nil
	})
	return out, err
}

// DismissSuggestion drops the enrichment value of field and remembers it, so
// a provider offering the same value again is ignored.
func (s *Store) DismissSuggestion(id, field, by string) error {
	if err := checkText("dismissed by", by, maxByBytes, false); err != nil {
		return err
	}
	return s.update(id, func(_ *bolt.Tx, p *Person) (bool, error) {
		i := p.factIndex(field, RankEnrichment)
		if i < 0 || p.Facts[i].Cleared() {
			return false, ErrNoSuggestion
		}
		value := p.Facts[i].Value
		p.Facts = slices.Delete(p.Facts, i, i+1)
		if !p.dismissed(field, value) {
			p.Dismissed = append(p.Dismissed, Dismissal{Field: field, Value: value, At: s.now().UTC(), By: by})
		}
		return true, nil
	})
}

// SetConsent records or replaces the consent for c.Scope. A zero UpdatedAt
// is stamped now.
func (s *Store) SetConsent(id string, c Consent) error {
	if err := checkConsent(c); err != nil {
		return err
	}
	if c.UpdatedAt.IsZero() {
		c.UpdatedAt = s.now().UTC()
	}
	return s.update(id, func(_ *bolt.Tx, p *Person) (bool, error) {
		if have, ok := p.Consent(c.Scope); ok && !c.Delivered() && len(c.DeliveredFields) == 0 && len(c.LastDeliveredFields) == 0 && c.DeliveredRev == "" {
			// A re-grant that says nothing about deliveries keeps the
			// delivery bookkeeping.
			c.DeliveredAt, c.FirstDeliveredAt = have.DeliveredAt, have.FirstDeliveredAt
			c.DeliveredFields, c.LastDeliveredFields, c.DeliveredRev = have.DeliveredFields, have.LastDeliveredFields, have.DeliveredRev
		}
		setConsent(p, c)
		return true, nil
	})
}

// DeleteConsent removes the consent record for scope, delivery bookkeeping
// included. It reports whether there was one.
func (s *Store) DeleteConsent(id, scope string) (bool, error) {
	var removed bool
	err := s.update(id, func(_ *bolt.Tx, p *Person) (bool, error) {
		before := len(p.Consents)
		p.Consents = slices.DeleteFunc(p.Consents, func(c Consent) bool { return c.Scope == scope })
		removed = len(p.Consents) != before
		return removed, nil
	})
	return removed, err
}

func checkConsent(c Consent) error {
	if err := checkText("consent scope", c.Scope, MaxIDBytes, true); err != nil {
		return err
	}
	if len(c.Fields) > maxConsentFields {
		return invalid("consent lists more than %d fields", maxConsentFields)
	}
	for _, list := range [][]string{c.Fields, c.DeliveredFields, c.LastDeliveredFields} {
		if len(list) > maxConsentFields {
			return invalid("consent lists more than %d fields", maxConsentFields)
		}
		for _, f := range list {
			if err := checkText("consent field", f, maxFieldBytes, true); err != nil {
				return err
			}
		}
	}
	if err := checkText("consent delivered rev", c.DeliveredRev, maxSourceBytes, false); err != nil {
		return err
	}
	return checkText("consent text version", c.TextVersion, maxSourceBytes, false)
}

func setConsent(p *Person, c Consent) {
	c.Fields = slices.Clone(c.Fields)
	c.DeliveredFields = slices.Clone(c.DeliveredFields)
	c.LastDeliveredFields = slices.Clone(c.LastDeliveredFields)
	for i := range p.Consents {
		if p.Consents[i].Scope == c.Scope {
			p.Consents[i] = c
			return
		}
	}
	p.Consents = append(p.Consents, c)
}

// WithdrawConsent stamps the consent for scope as withdrawn now.
func (s *Store) WithdrawConsent(id, scope string) error {
	return s.updateConsent(id, scope, func(c *Consent) {
		now := s.now().UTC()
		c.WithdrawnAt, c.UpdatedAt = now, now
	})
}

// MarkConsentDelivered stamps the consent for scope as delivered now. Use
// RecordDelivery to also record what was delivered.
func (s *Store) MarkConsentDelivered(id, scope string) error {
	return s.updateConsent(id, scope, func(c *Consent) {
		c.DeliveredAt = s.now().UTC()
		if c.FirstDeliveredAt.IsZero() {
			c.FirstDeliveredAt = c.DeliveredAt
		}
	})
}

// RecordDelivery records one delivery under the consent for scope: the time
// (the first delivery is kept as FirstDeliveredAt), the fields it carried
// (added to DeliveredFields, and kept as LastDeliveredFields) and its
// revision.
func (s *Store) RecordDelivery(id, scope string, d Delivery) error {
	if len(d.Fields) > maxConsentFields {
		return invalid("delivery lists more than %d fields", maxConsentFields)
	}
	for _, f := range d.Fields {
		if err := checkText("delivered field", f, maxFieldBytes, true); err != nil {
			return err
		}
	}
	if err := checkText("delivered rev", d.Rev, maxSourceBytes, false); err != nil {
		return err
	}
	at := d.At.UTC()
	if d.At.IsZero() {
		at = s.now().UTC()
	}
	return s.updateConsent(id, scope, func(c *Consent) {
		c.DeliveredAt = at
		if c.FirstDeliveredAt.IsZero() {
			c.FirstDeliveredAt = at
		}
		for _, f := range d.Fields {
			// Bounded like every field list, so the record always passes
			// checkConsent on a restore.
			if !slices.Contains(c.DeliveredFields, f) && len(c.DeliveredFields) < maxConsentFields {
				c.DeliveredFields = append(c.DeliveredFields, f)
			}
		}
		c.LastDeliveredFields = slices.Clone(d.Fields)
		c.DeliveredRev = d.Rev
	})
}

func (s *Store) updateConsent(id, scope string, fn func(*Consent)) error {
	return s.update(id, func(_ *bolt.Tx, p *Person) (bool, error) {
		for i := range p.Consents {
			if p.Consents[i].Scope == scope {
				fn(&p.Consents[i])
				return true, nil
			}
		}
		return false, ErrNoConsent
	})
}

// Forget erases the person completely: the record, every fact, consent and
// dismissal, and the identity index. The person id and each identity are
// replaced by salted hash tombstones, so neither Upsert, Import nor Migrate
// brings them back. It returns the number of identity tombstones written.
//
// bbolt does not zero freed pages: the old record's bytes stay in the file,
// unreachable, until the pages are reused. A consumer that must also scrub
// the file (and its backups) compacts it, for example with "bbolt compact".
func (s *Store) Forget(id string) (int, error) {
	n := 0
	err := s.db.Update(func(tx *bolt.Tx) error {
		var err error
		n, err = s.forgetTx(tx, id)
		return err
	})
	return n, err
}

func (s *Store) forgetTx(tx *bolt.Tx, id string) (int, error) {
	p, ok, err := loadPerson(tx, id)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, ErrNotFound
	}
	n := 0
	for _, ident := range p.Identities {
		if err := s.tombstoneTx(tx, ident.Ref()); err != nil {
			return 0, err
		}
		// Nobody keeps a forgotten identity, held or not.
		if err := s.dropHoldersTx(tx, ident.Ref()); err != nil {
			return 0, err
		}
		if err := tx.Bucket(bucketIdentities).Delete(indexKey(ident.Ref())); err != nil {
			return 0, err
		}
		n++
	}
	// A held identity belongs to its owner, so it is not tombstoned: only
	// this person's claim to it goes.
	for _, h := range p.Held {
		if err := heldIndexRemove(tx, h.Ref(), id); err != nil {
			return 0, err
		}
	}
	if err := tx.Bucket(bucketTombstones).Put(personTombstoneKey(tx, id), s.stamp()); err != nil {
		return 0, err
	}
	return n, tx.Bucket(bucketPeople).Delete([]byte(id))
}

// ForgetIdentity tombstones one identity, whether or not anyone holds it. A
// person holding it loses the identity and every fact derived from it, and
// every person holding it as a HeldIdentity loses that.
func (s *Store) ForgetIdentity(ref IdentityRef) error {
	if err := checkRef(ref); err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error { return s.forgetIdentityTx(tx, ref) })
}

// forgetIdentityTx is ForgetIdentity inside tx.
func (s *Store) forgetIdentityTx(tx *bolt.Tx, ref IdentityRef) error {
	if err := s.tombstoneTx(tx, ref); err != nil {
		return err
	}
	if err := s.dropHoldersTx(tx, ref); err != nil {
		return err
	}
	index := tx.Bucket(bucketIdentities)
	owner := index.Get(indexKey(ref))
	if owner == nil {
		return nil
	}
	id := string(owner)
	if err := index.Delete(indexKey(ref)); err != nil {
		return err
	}
	p, ok, err := loadPerson(tx, id)
	if err != nil || !ok {
		return err
	}
	removeIdentity(&p, ref)
	dropDerived(&p, ref)
	p.UpdatedAt = s.now().UTC()
	return putPerson(tx, p)
}

// dropHoldersTx removes a forgotten identity from everyone holding it, with
// every fact they derived from it, and drops its held-index entry.
func (s *Store) dropHoldersTx(tx *bolt.Tx, ref IdentityRef) error {
	holders, err := heldIndexList(tx, ref)
	if err != nil {
		return err
	}
	for _, holder := range holders {
		hp, ok, err := loadPerson(tx, holder)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if err := dropHeldTx(tx, &hp, ref); err != nil {
			return err
		}
		dropDerived(&hp, ref)
		hp.UpdatedAt = s.now().UTC()
		if err := putPerson(tx, hp); err != nil {
			return err
		}
	}
	return tx.Bucket(bucketHeld).Delete(indexKey(ref))
}

// dropDerived removes every fact of p derived from ref.
func dropDerived(p *Person, ref IdentityRef) {
	p.Facts = slices.DeleteFunc(p.Facts, func(f Fact) bool {
		return slices.ContainsFunc(f.DerivedFrom, func(d IdentityRef) bool {
			return identityClass(d.Kind) == identityClass(ref.Kind) && d.Value == normalizeValue(ref.Kind, ref.Value)
		})
	})
}

// IsTombstoned reports whether the identity was forgotten.
func (s *Store) IsTombstoned(ref IdentityRef) (bool, error) {
	if err := checkRef(ref); err != nil {
		return false, err
	}
	var tomb bool
	err := s.db.View(func(tx *bolt.Tx) error {
		var err error
		tomb, err = s.tombstonedTx(tx, ref)
		return err
	})
	return tomb, err
}

// tombstoneKey hashes an identity with the salt stored in the file. The
// salt is read inside the transaction, so an Import that adopts a
// snapshot's salt hashes the rest of that transaction with it.
func tombstoneKey(tx *bolt.Tx, ref IdentityRef) []byte {
	in := slices.Concat(tx.Bucket(bucketMeta).Get(keySalt), []byte(identityClass(ref.Kind)), []byte{0}, []byte(normalizeValue(ref.Kind, ref.Value)))
	sum := sha256.Sum256(in)
	return []byte(hex.EncodeToString(sum[:]))
}

func (s *Store) tombstoneTx(tx *bolt.Tx, ref IdentityRef) error {
	return tx.Bucket(bucketTombstones).Put(tombstoneKey(tx, ref), s.stamp())
}

func (s *Store) stamp() []byte { return []byte(s.now().UTC().Format(time.RFC3339)) }

// personTombstoneKey hashes a forgotten person id in its own namespace.
func personTombstoneKey(tx *bolt.Tx, id string) []byte {
	in := slices.Concat(tx.Bucket(bucketMeta).Get(keySalt), []byte(personTombstoneClass), []byte{0}, []byte(id))
	sum := sha256.Sum256(in)
	return []byte(hex.EncodeToString(sum[:]))
}

func (s *Store) tombstonedTx(tx *bolt.Tx, ref IdentityRef) (bool, error) {
	return tx.Bucket(bucketTombstones).Get(tombstoneKey(tx, ref)) != nil, nil
}

// Stats counts what the store holds. It never returns values, so a parity
// check can log it.
type Stats struct {
	People      int          `json:"people"`
	Identities  int          `json:"identities"`
	Facts       map[Rank]int `json:"facts"`
	Suggestions int          `json:"suggestions"`
	Consents    int          `json:"consents"`
	Tombstones  int          `json:"tombstones"`
	// Held counts held identities, Answers provider answers, and
	// Attachments the people that carry at least one attachment.
	Held        int `json:"held"`
	Answers     int `json:"answers"`
	Attachments int `json:"attachments"`
}

// Stats returns the counts.
func (s *Store) Stats() (Stats, error) {
	st := Stats{Facts: map[Rank]int{}}
	err := s.db.View(func(tx *bolt.Tx) error {
		st.Tombstones = tx.Bucket(bucketTombstones).Stats().KeyN
		return tx.Bucket(bucketPeople).ForEach(func(_, v []byte) error {
			var p Person
			if err := json.Unmarshal(v, &p); err != nil {
				return fmt.Errorf("store: decode person: %w", err)
			}
			st.People++
			st.Identities += len(p.Identities)
			st.Consents += len(p.Consents)
			st.Held += len(p.Held)
			st.Answers += len(p.Answers)
			if len(p.Attachments) > 0 {
				st.Attachments++
			}
			for _, f := range p.Facts {
				st.Facts[f.Rank]++
				if f.Suggested {
					st.Suggestions++
				}
			}
			return nil
		})
	})
	return st, err
}

// update loads person id, applies fn and writes the person back when fn
// reports a change. It runs in one transaction.
func (s *Store) update(id string, fn func(tx *bolt.Tx, p *Person) (bool, error)) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		p, ok, err := loadPerson(tx, id)
		if err != nil {
			return err
		}
		if !ok {
			return ErrNotFound
		}
		changed, err := fn(tx, &p)
		if err != nil || !changed {
			return err
		}
		p.UpdatedAt = s.now().UTC()
		return putPerson(tx, p)
	})
}

func loadPerson(tx *bolt.Tx, id string) (Person, bool, error) {
	raw := tx.Bucket(bucketPeople).Get([]byte(id))
	if raw == nil {
		return Person{}, false, nil
	}
	var p Person
	if err := json.Unmarshal(raw, &p); err != nil {
		return Person{}, false, fmt.Errorf("store: decode person: %w", err)
	}
	return p, true, nil
}

func putPerson(tx *bolt.Tx, p Person) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return tx.Bucket(bucketPeople).Put([]byte(p.ID), raw)
}

func indexKey(ref IdentityRef) []byte {
	return []byte(identityClass(ref.Kind) + "\x00" + normalizeValue(ref.Kind, ref.Value))
}

func newID() (string, error) {
	b := make([]byte, generatedIDBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
