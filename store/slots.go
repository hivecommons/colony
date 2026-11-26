package store

// Schema version 2 slots: held identities, provider answers, attachments,
// the exact-state write (Replace) and multi-person transactions (Batch).

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/hivecommons/colony/resolver"
	bolt "go.etcd.io/bbolt"
)

// Contest names everyone who claims one identity.
type Contest struct {
	// Owner is the person the identity index gives it to, or empty.
	Owner string `json:"owner,omitempty"`
	// Holders are the people holding it as a HeldIdentity, in id order.
	Holders []string `json:"holders,omitempty"`
}

// Contested reports whether more than one person claims the identity.
func (c Contest) Contested() bool {
	n := len(c.Holders)
	if c.Owner != "" {
		n++
	}
	return n > 1
}

// Contested returns who claims an identity: its owner and every holder.
func (s *Store) Contested(ref IdentityRef) (Contest, error) {
	if err := checkRef(ref); err != nil {
		return Contest{}, err
	}
	var c Contest
	err := s.db.View(func(tx *bolt.Tx) error {
		c.Owner = string(tx.Bucket(bucketIdentities).Get(indexKey(ref)))
		c.Holders = heldIndexList(tx, ref)
		return nil
	})
	return c, err
}

// RemoveHeld drops a held identity from a person. It reports whether the
// person held it.
func (s *Store) RemoveHeld(id string, ref IdentityRef) (bool, error) {
	if err := checkRef(ref); err != nil {
		return false, err
	}
	var removed bool
	err := s.update(id, func(tx *bolt.Tx, p *Person) (bool, error) {
		removed = p.HoldsIdentity(ref)
		if !removed {
			return false, nil
		}
		return true, dropHeldTx(tx, p, ref)
	})
	return removed, err
}

// holdTx adds or updates a held identity on p and indexes it. The caller
// writes p.
func holdTx(tx *bolt.Tx, p *Person, h HeldIdentity) error {
	if i := p.heldIndex(h.Ref()); i >= 0 {
		have := p.Held[i]
		h.Kind = have.Kind
		h.Verified = h.Verified || have.Verified
		h.Manual = h.Manual || have.Manual
		if h.Source == "" {
			h.Source = have.Source
		}
		if h.By == "" {
			h.By = have.By
		}
		if h.Score == 0 {
			h.Score = have.Score
		}
		p.Held[i] = h
	} else {
		p.Held = append(p.Held, h)
	}
	return heldIndexAdd(tx, h.Ref(), p.ID)
}

// dropHeldTx removes a held identity from p and from the index. The caller
// writes p.
func dropHeldTx(tx *bolt.Tx, p *Person, ref IdentityRef) error {
	i := p.heldIndex(ref)
	if i < 0 {
		return nil
	}
	p.Held = slices.Delete(p.Held, i, i+1)
	return heldIndexRemove(tx, ref, p.ID)
}

func heldIndexList(tx *bolt.Tx, ref IdentityRef) []string {
	raw := tx.Bucket(bucketHeld).Get(indexKey(ref))
	if raw == nil {
		return nil
	}
	var ids []string
	if json.Unmarshal(raw, &ids) != nil {
		return nil
	}
	return ids
}

func heldIndexPut(tx *bolt.Tx, ref IdentityRef, ids []string) error {
	if len(ids) == 0 {
		return tx.Bucket(bucketHeld).Delete(indexKey(ref))
	}
	sort.Strings(ids)
	raw, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	return tx.Bucket(bucketHeld).Put(indexKey(ref), raw)
}

func heldIndexAdd(tx *bolt.Tx, ref IdentityRef, id string) error {
	ids := heldIndexList(tx, ref)
	if slices.Contains(ids, id) {
		return nil
	}
	return heldIndexPut(tx, ref, append(ids, id))
}

func heldIndexRemove(tx *bolt.Tx, ref IdentityRef, id string) error {
	ids := heldIndexList(tx, ref)
	if !slices.Contains(ids, id) {
		return nil
	}
	return heldIndexPut(tx, ref, slices.DeleteFunc(ids, func(v string) bool { return v == id }))
}

// SetAnswer records a provider's answer about a person, replacing the
// earlier answer from the same source. An answer older than the stored one
// is ignored; it reports whether a was stored.
func (s *Store) SetAnswer(id string, a Answer) (bool, error) {
	a, err := s.prepareAnswer(a)
	if err != nil {
		return false, err
	}
	var stored bool
	err = s.update(id, func(_ *bolt.Tx, p *Person) (bool, error) {
		var err error
		stored, err = setAnswer(p, a)
		return stored, err
	})
	return stored, err
}

// DeleteAnswer drops the answer from source. It returns ErrNoAnswer when
// there is none.
func (s *Store) DeleteAnswer(id, source string) error {
	return s.update(id, func(_ *bolt.Tx, p *Person) (bool, error) {
		before := len(p.Answers)
		p.Answers = slices.DeleteFunc(p.Answers, func(a Answer) bool { return a.Source == source })
		if len(p.Answers) == before {
			return false, ErrNoAnswer
		}
		return true, nil
	})
}

func setAnswer(p *Person, a Answer) (bool, error) {
	for i := range p.Answers {
		if p.Answers[i].Source == a.Source {
			if a.FetchedAt.Before(p.Answers[i].FetchedAt) {
				return false, nil
			}
			p.Answers[i] = a
			return true, nil
		}
	}
	if len(p.Answers) >= maxAnswers {
		return false, invalid("more than %d answers", maxAnswers)
	}
	p.Answers = append(p.Answers, a)
	return true, nil
}

// prepareAnswer validates an answer and returns a normalised copy.
func (s *Store) prepareAnswer(in Answer) (Answer, error) {
	if err := checkText("answer source", in.Source, maxSourceBytes, true); err != nil {
		return Answer{}, err
	}
	if len(in.Affiliations) > maxAnswerAffs {
		return Answer{}, invalid("an answer holds more than %d affiliations", maxAnswerAffs)
	}
	if len(in.Activity) > maxAnswerActs {
		return Answer{}, invalid("an answer holds more than %d activities", maxAnswerActs)
	}
	out := Answer{Source: in.Source, FetchedAt: in.FetchedAt.UTC()}
	if in.FetchedAt.IsZero() {
		out.FetchedAt = s.now().UTC()
	}
	for _, a := range in.Affiliations {
		if err := checkText("answer org", a.Org, maxAnswerText, true); err != nil {
			return Answer{}, err
		}
		if err := checkText("answer affiliation source", a.Source, maxSourceBytes, false); err != nil {
			return Answer{}, err
		}
		if err := checkText("answer matched_by", a.MatchedBy, maxFieldBytes, false); err != nil {
			return Answer{}, err
		}
		switch a.Confidence {
		case "", resolver.ConfidenceHigh, resolver.ConfidenceMedium, resolver.ConfidenceLow:
		default:
			return Answer{}, invalid("unknown confidence %q", a.Confidence)
		}
		if !a.From.IsZero() && !a.Until.IsZero() && a.Until.Before(a.From) {
			return Answer{}, invalid("answer affiliation until is before from")
		}
		if !a.From.IsZero() {
			a.From = a.From.UTC()
		}
		if !a.Until.IsZero() {
			a.Until = a.Until.UTC()
		}
		out.Affiliations = append(out.Affiliations, a)
	}
	for _, a := range in.Activity {
		if err := checkText("answer project", a.Project, maxAnswerText, true); err != nil {
			return Answer{}, err
		}
		if err := checkText("answer period", a.Period, maxAnswerText, false); err != nil {
			return Answer{}, err
		}
		if err := checkText("answer activity source", a.Source, maxSourceBytes, false); err != nil {
			return Answer{}, err
		}
		if err := checkText("answer url", a.URL, maxAnswerURL, false); err != nil {
			return Answer{}, err
		}
		if a.URL != "" && !strings.HasPrefix(a.URL, "https://") && !strings.HasPrefix(a.URL, "http://") {
			return Answer{}, invalid("answer url is not http(s)")
		}
		if a.Contributions < 0 {
			return Answer{}, invalid("negative contribution count")
		}
		out.Activity = append(out.Activity, a)
	}
	return out, nil
}

// SetAttachment stores data under namespace ns on a person; empty data
// removes it. data must be a JSON document, and all of a person's
// attachments together at most MaxAttachmentBytes.
func (s *Store) SetAttachment(id, ns string, data json.RawMessage) error {
	if err := checkAttachment(ns, data); err != nil {
		return err
	}
	return s.update(id, func(_ *bolt.Tx, p *Person) (bool, error) {
		return true, setAttachment(p, ns, data)
	})
}

func checkAttachment(ns string, data json.RawMessage) error {
	if err := checkText("attachment namespace", ns, maxFieldBytes, true); err != nil {
		return err
	}
	if len(data) > 0 && !json.Valid(data) {
		return invalid("attachment %q is not a JSON document", ns)
	}
	return nil
}

func setAttachment(p *Person, ns string, data json.RawMessage) error {
	if len(data) == 0 {
		delete(p.Attachments, ns)
		if len(p.Attachments) == 0 {
			p.Attachments = nil
		}
		return nil
	}
	if p.Attachments == nil {
		p.Attachments = map[string]json.RawMessage{}
	}
	p.Attachments[ns] = slices.Clone(data)
	total := 0
	for k, v := range p.Attachments {
		total += len(k) + len(v)
	}
	if total > MaxAttachmentBytes {
		return invalid("attachments are larger than %d bytes", MaxAttachmentBytes)
	}
	return nil
}

// SetStoreAttachment stores data under namespace ns for the whole store,
// not for one person: consumer data that belongs to the deployment. Empty
// data removes it. An unfiltered Export carries it; a filtered one never
// does. Forget does not touch it, so it must not hold anything about one
// person that a forget would have to erase.
func (s *Store) SetStoreAttachment(ns string, data json.RawMessage) error {
	return s.db.Update(func(tx *bolt.Tx) error { return setStoreAttachmentTx(tx, ns, data) })
}

func setStoreAttachmentTx(tx *bolt.Tx, ns string, data json.RawMessage) error {
	if err := checkAttachment(ns, data); err != nil {
		return err
	}
	if len(data) > MaxStoreAttachmentBytes {
		return invalid("store attachment %q is larger than %d bytes", ns, MaxStoreAttachmentBytes)
	}
	key := []byte(attachmentPrefix + ns)
	if len(data) == 0 {
		return tx.Bucket(bucketMeta).Delete(key)
	}
	return tx.Bucket(bucketMeta).Put(key, slices.Clone(data))
}

// StoreAttachment returns the store-level attachment under ns.
func (s *Store) StoreAttachment(ns string) (json.RawMessage, bool, error) {
	var out json.RawMessage
	err := s.db.View(func(tx *bolt.Tx) error {
		if raw := tx.Bucket(bucketMeta).Get([]byte(attachmentPrefix + ns)); raw != nil {
			out = slices.Clone(raw)
		}
		return nil
	})
	return out, out != nil, err
}

func storeAttachmentsTx(tx *bolt.Tx) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	c := tx.Bucket(bucketMeta).Cursor()
	prefix := []byte(attachmentPrefix)
	for k, v := c.Seek(prefix); k != nil && strings.HasPrefix(string(k), attachmentPrefix); k, v = c.Next() {
		out[strings.TrimPrefix(string(k), attachmentPrefix)] = slices.Clone(v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ReplaceReport counts what Replace did. It holds no values.
type ReplaceReport struct {
	Identities int `json:"identities"`
	Held       int `json:"held"`
	Facts      int `json:"facts"`
	// SkippedIdentities were forgotten before and were not written.
	SkippedIdentities int `json:"skipped_identities"`
	// SkippedFacts were derived from a skipped identity, or were a
	// dismissed enrichment value.
	SkippedFacts int `json:"skipped_facts"`
}

// Replace sets the person rec.Person.ID to exactly what rec holds: status,
// identities, held identities, facts, dismissals, consents, answers and
// attachments. Whatever the stored record holds that rec does not is
// removed. It is the write for a consumer whose own copy is the truth, for
// example a full sync, and unlike Import it does not merge.
//
// The store's rules still apply: an identity another person owns becomes a
// held identity (as with HoldConflicts), a forgotten identity is skipped and
// so is every fact derived from it, a dismissed enrichment value is
// skipped, and values are validated and their locality forced as SetFact
// does. rec.Forgotten is tombstoned first. A forgotten person id fails with
// ErrTombstoned, and two facts for the same field and rank with ErrInvalid.
func (s *Store) Replace(rec Record) (ReplaceReport, error) {
	var rep ReplaceReport
	err := s.Batch(func(b *Batch) error {
		var err error
		rep, err = b.Replace(rec)
		return err
	})
	return rep, err
}

func (s *Store) replaceTx(tx *bolt.Tx, rec Record) (ReplaceReport, error) {
	var rep ReplaceReport
	for _, ref := range rec.Forgotten {
		if err := checkRef(ref); err != nil {
			return rep, err
		}
		if err := s.tombstoneTx(tx, ref); err != nil {
			return rep, err
		}
	}
	id := rec.Person.ID
	if err := checkID(id); err != nil {
		return rep, err
	}
	if tx.Bucket(bucketTombstones).Get(personTombstoneKey(tx, id)) != nil {
		return rep, fmt.Errorf("%w: person id", ErrTombstoned)
	}
	if rec.Person.Status != "" {
		if err := checkText("status", rec.Person.Status, maxStatusBytes, false); err != nil {
			return rep, err
		}
	}
	now := s.now().UTC()
	old, existed, err := loadPerson(tx, id)
	if err != nil {
		return rep, err
	}
	// Release what the stored record holds; it is written again below.
	index := tx.Bucket(bucketIdentities)
	for _, ident := range old.Identities {
		if string(index.Get(indexKey(ident.Ref()))) == id {
			if err := index.Delete(indexKey(ident.Ref())); err != nil {
				return rep, err
			}
		}
	}
	for _, h := range old.Held {
		if err := heldIndexRemove(tx, h.Ref(), id); err != nil {
			return rep, err
		}
	}
	p := Person{ID: id, Status: rec.Person.Status, CreatedAt: old.CreatedAt, UpdatedAt: now}
	if p.Status == "" {
		p.Status = StatusActive
	}
	if !existed || p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	if !rec.CreatedAt.IsZero() && rec.CreatedAt.Before(p.CreatedAt) {
		p.CreatedAt = rec.CreatedAt.UTC()
	}
	var skipped []IdentityRef
	var held []Identity
	for _, in := range rec.Person.Identities {
		ident, err := s.prepareIdentity(in, now)
		if err != nil {
			return rep, err
		}
		tomb, err := s.tombstonedTx(tx, ident.Ref())
		if err != nil {
			return rep, err
		}
		if tomb {
			skipped = append(skipped, ident.Ref())
			rep.SkippedIdentities++
			continue
		}
		if owner := index.Get(indexKey(ident.Ref())); owner != nil && string(owner) != id {
			held = append(held, ident)
			continue
		}
		mergeIdentity(&p, ident, true)
		if err := index.Put(indexKey(ident.Ref()), []byte(id)); err != nil {
			return rep, err
		}
	}
	rep.Identities = len(p.Identities)
	for _, in := range rec.Person.Held {
		ident, err := s.prepareIdentity(in, now)
		if err != nil {
			return rep, err
		}
		held = append(held, ident)
	}
	for _, ident := range held {
		tomb, err := s.tombstonedTx(tx, ident.Ref())
		if err != nil {
			return rep, err
		}
		if tomb {
			skipped = append(skipped, ident.Ref())
			rep.SkippedIdentities++
			continue
		}
		if p.HasIdentity(ident.Ref()) {
			continue
		}
		owner := string(index.Get(indexKey(ident.Ref())))
		if err := holdTx(tx, &p, HeldIdentity{Identity: ident, Owner: owner}); err != nil {
			return rep, err
		}
	}
	if len(p.Held) > maxHeld {
		return rep, invalid("more than %d held identities", maxHeld)
	}
	rep.Held = len(p.Held)
	for _, d := range rec.Dismissed {
		if err := checkDismissal(d); err != nil {
			return rep, err
		}
		if !p.dismissed(d.Field, d.Value) {
			d.At = d.At.UTC()
			p.Dismissed = append(p.Dismissed, d)
		}
	}
	seen := map[string]bool{}
	for _, f := range rec.Facts {
		key := f.Field + "\x00" + string(f.Rank)
		if seen[key] {
			return rep, invalid("two facts for field %q at rank %q", f.Field, f.Rank)
		}
		seen[key] = true
		if slices.ContainsFunc(f.DerivedFrom, func(d IdentityRef) bool { return slices.ContainsFunc(skipped, sameIdentity(d)) }) {
			rep.SkippedFacts++
			continue
		}
		res, err := s.setFact(&p, f)
		if err != nil {
			return rep, err
		}
		if res.Stored {
			rep.Facts++
		} else {
			rep.SkippedFacts++
		}
	}
	for _, c := range rec.Consents {
		if err := checkConsent(c); err != nil {
			return rep, err
		}
		if _, dup := p.Consent(c.Scope); dup {
			return rep, invalid("two consents for one scope")
		}
		setConsent(&p, c)
	}
	sources := map[string]bool{}
	for _, in := range rec.Answers {
		a, err := s.prepareAnswer(in)
		if err != nil {
			return rep, err
		}
		if sources[a.Source] {
			return rep, invalid("two answers from source %q", a.Source)
		}
		sources[a.Source] = true
		if _, err := setAnswer(&p, a); err != nil {
			return rep, err
		}
	}
	names := make([]string, 0, len(rec.Attachments))
	for ns := range rec.Attachments {
		names = append(names, ns)
	}
	sort.Strings(names)
	for _, ns := range names {
		if err := checkAttachment(ns, rec.Attachments[ns]); err != nil {
			return rep, err
		}
		if err := setAttachment(&p, ns, rec.Attachments[ns]); err != nil {
			return rep, err
		}
	}
	return rep, putPerson(tx, p)
}

func checkDismissal(d Dismissal) error {
	if err := checkText("dismissed field", d.Field, maxFieldBytes, true); err != nil {
		return err
	}
	if err := checkText("dismissed value", d.Value, MaxValueBytes, true); err != nil {
		return err
	}
	return checkText("dismissed by", d.By, maxByBytes, false)
}

// Batch is a set of writes that commit together: when the function given to
// Store.Batch returns an error, none of them is written. A Batch is only
// valid inside that function.
type Batch struct {
	s  *Store
	tx *bolt.Tx
}

// Batch runs fn in one write transaction. fn must not call back into the
// Store itself, only into the Batch.
func (s *Store) Batch(fn func(b *Batch) error) error {
	return s.db.Update(func(tx *bolt.Tx) error { return fn(&Batch{s: s, tx: tx}) })
}

// Get returns the person with id as the batch sees it, or ErrNotFound.
func (b *Batch) Get(id string) (Person, error) {
	p, ok, err := loadPerson(b.tx, id)
	if err == nil && !ok {
		err = ErrNotFound
	}
	return p, err
}

// Replace is Store.Replace inside the batch.
func (b *Batch) Replace(rec Record) (ReplaceReport, error) { return b.s.replaceTx(b.tx, rec) }

// Upsert is Store.Upsert inside the batch.
func (b *Batch) Upsert(spec PersonSpec) (Person, error) { return b.s.upsertTx(b.tx, spec) }

// Forget is Store.Forget inside the batch.
func (b *Batch) Forget(id string) (int, error) { return b.s.forgetTx(b.tx, id) }

// RemoveIdentity is Store.RemoveIdentity inside the batch.
func (b *Batch) RemoveIdentity(id string, ref IdentityRef) error {
	if err := checkRef(ref); err != nil {
		return err
	}
	p, ok, err := loadPerson(b.tx, id)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	if !removeIdentity(&p, ref) {
		return nil
	}
	if err := b.tx.Bucket(bucketIdentities).Delete(indexKey(ref)); err != nil {
		return err
	}
	p.UpdatedAt = b.s.now().UTC()
	return putPerson(b.tx, p)
}

// SetStoreAttachment is Store.SetStoreAttachment inside the batch.
func (b *Batch) SetStoreAttachment(ns string, data json.RawMessage) error {
	return setStoreAttachmentTx(b.tx, ns, data)
}

// SetAttachment is Store.SetAttachment inside the batch.
func (b *Batch) SetAttachment(id, ns string, data json.RawMessage) error {
	if err := checkAttachment(ns, data); err != nil {
		return err
	}
	p, ok, err := loadPerson(b.tx, id)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	if err := setAttachment(&p, ns, data); err != nil {
		return err
	}
	p.UpdatedAt = b.s.now().UTC()
	return putPerson(b.tx, p)
}
