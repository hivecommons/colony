package store

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hivecommons/colony/resolver"
)

// Kind is the kind of an identity.
type Kind string

const (
	// KindGitHub is a github.com login.
	KindGitHub Kind = "github"
	// KindGHE is a GitHub Enterprise login, conventionally "host/login".
	KindGHE Kind = "ghe"
	// KindEmail is an email address the person gave or confirmed.
	KindEmail Kind = "email"
	// KindGitEmail is an email address seen as a git commit author.
	KindGitEmail Kind = "git_email"
	// KindSlack is a Slack member id.
	KindSlack Kind = "slack"
	// KindOIDC is an OIDC subject or a consumer's canonical account id.
	KindOIDC Kind = "oidc"
)

var validKinds = []Kind{KindGitHub, KindGHE, KindEmail, KindGitEmail, KindSlack, KindOIDC}

// Valid reports whether k is a known identity kind.
func (k Kind) Valid() bool { return slices.Contains(validKinds, k) }

// IsEmail reports whether k holds an email address.
func (k Kind) IsEmail() bool { return k == KindEmail || k == KindGitEmail }

// Locality says whether a value may ever leave the deployment that holds it.
type Locality string

const (
	// LocalityShareable may be sent to another deployment inside the
	// consumer's trust domain, labelled with its rank and source.
	LocalityShareable Locality = "shareable"
	// LocalityLocalOnly never leaves the deployment that collected it.
	// Every email on a domain that is not free-mail is local_only, and so is
	// every fact derived from a local_only identity.
	LocalityLocalOnly Locality = "local_only"
)

// Valid reports whether l is a known locality. The empty locality is valid
// and is stored as LocalityLocalOnly (fail closed).
func (l Locality) Valid() bool {
	return l == "" || l == LocalityShareable || l == LocalityLocalOnly
}

func (l Locality) orLocal() Locality {
	if l == LocalityShareable {
		return LocalityShareable
	}
	return LocalityLocalOnly
}

// Rank is who set a fact. It decides precedence: RankUser > RankAdmin >
// RankHubShared > RankEnrichment.
type Rank string

const (
	// RankUser is a value the person, or a deployment's own user, set.
	RankUser Rank = "user"
	// RankAdmin is a value an administrator set.
	RankAdmin Rank = "admin"
	// RankHubShared is a value received from a trusted hub, labelled with
	// its origin. Its Source starts with HubSourcePrefix.
	RankHubShared Rank = "hub_shared"
	// RankEnrichment is a derived value, for example from a colony
	// provider or a forge profile. It may be a suggestion.
	RankEnrichment Rank = "enrichment"
)

// rankOrder lists the ranks strongest first.
var rankOrder = []Rank{RankUser, RankAdmin, RankHubShared, RankEnrichment}

// Ranks returns every rank, strongest first.
func Ranks() []Rank { return slices.Clone(rankOrder) }

// Valid reports whether r is a known rank.
func (r Rank) Valid() bool { return slices.Contains(rankOrder, r) }

// Outranks reports whether r takes precedence over other.
func (r Rank) Outranks(other Rank) bool {
	return r.strength() > other.strength()
}

// strength is higher for a stronger rank, and below every valid rank for an
// unknown one.
func (r Rank) strength() int {
	i := slices.Index(rankOrder, r)
	if i < 0 {
		return i
	}
	return len(rankOrder) - i
}

// HubSourcePrefix starts the Source of every RankHubShared fact, so the
// label of the hub value is always visible ("hub:person", "hub:email_domain").
const HubSourcePrefix = "hub:"

// ColonySourcePrefix starts the Source of every fact FactsFromResult builds
// from a colony provider ("colony:gitdm", "colony:devstats").
const ColonySourcePrefix = "colony:"

// Well-known fact fields. Other fields are allowed.
const (
	FieldRealName     = "real_name"
	FieldOrganization = "organization"
	FieldAffiliation  = "affiliation"
	FieldCountry      = "country"
	FieldLocation     = "location"
	FieldNotes        = "notes"
	FieldSlackID      = "slack_id"
	// FieldActivityPrefix starts a per-project contribution count field,
	// for example "activity:kubernetes".
	FieldActivityPrefix = "activity:"
)

// alwaysLocalFields are stored local_only whatever the caller asks for:
// administrator notes and chat ids never leave the deployment.
var alwaysLocalFields = []string{FieldNotes, FieldSlackID}

// ConsentFirstFields are only shared under an active Consent that covers
// them. See ConsentFirst.
var ConsentFirstFields = []string{FieldRealName, FieldLocation}

// StatusActive is the status a new person gets when the caller sets none.
const StatusActive = "active"

// Size limits keep one person record small and keys inside bbolt's limits.
const (
	maxIDBytes       = 128
	maxFieldBytes    = 64
	maxSourceBytes   = 128
	maxStatusBytes   = 64
	maxIdentityBytes = 512
	maxValueBytes    = 4096
	maxByBytes       = 256
	maxConsentFields = 64
)

// Errors returned by the store.
var (
	ErrNotFound         = errors.New("store: person not found")
	ErrInvalid          = errors.New("store: invalid input")
	ErrTombstoned       = errors.New("store: identity was forgotten and cannot be re-added")
	ErrIdentityConflict = errors.New("store: identity belongs to another person")
	ErrSchemaTooNew     = errors.New("store: file schema is newer than this colony release")
	ErrSaltMismatch     = errors.New("store: snapshot tombstones were hashed with another salt")
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// IdentityRef names an identity without its metadata.
type IdentityRef struct {
	Kind  Kind   `json:"kind"`
	Value string `json:"value"`
}

// Identity is one way to recognise a person.
type Identity struct {
	Kind     Kind      `json:"kind"`
	Value    string    `json:"value"`
	Verified bool      `json:"verified,omitempty"`
	Locality Locality  `json:"locality"`
	Source   string    `json:"source,omitempty"`
	At       time.Time `json:"at,omitzero"`
}

// Ref returns the identity's reference.
func (i Identity) Ref() IdentityRef { return IdentityRef{Kind: i.Kind, Value: i.Value} }

// Fact is one value of one field at one rank, with its provenance.
//
// An empty Value is an explicit clear at that rank: it hides every lower
// rank. From and Until bound the period the value applies to (Until is
// exclusive); At is when it was set and By who set it.
type Fact struct {
	Field      string              `json:"field"`
	Value      string              `json:"value"`
	Rank       Rank                `json:"rank"`
	Source     string              `json:"source"`
	Confidence resolver.Confidence `json:"confidence,omitempty"`
	Locality   Locality            `json:"locality"`
	From       time.Time           `json:"from,omitzero"`
	Until      time.Time           `json:"until,omitzero"`
	At         time.Time           `json:"at,omitzero"`
	By         string              `json:"by,omitempty"`
	// Suggested marks an enrichment value nobody accepted yet.
	Suggested bool `json:"suggested,omitempty"`
	// AcceptedAt and AcceptedBy record who accepted a suggestion.
	AcceptedAt time.Time `json:"accepted_at,omitzero"`
	AcceptedBy string    `json:"accepted_by,omitempty"`
	// DerivedFrom lists the identities the value was derived from. A fact
	// derived from a local_only identity, or from one the person no longer
	// holds, is local_only.
	DerivedFrom []IdentityRef `json:"derived_from,omitempty"`
}

// Cleared reports whether the fact is an explicit clear.
func (f Fact) Cleared() bool { return f.Value == "" }

// Consent is one person's consent to share fields with one scope (a hive id,
// or "hub").
type Consent struct {
	Scope       string    `json:"scope"`
	Fields      []string  `json:"fields"`
	TextVersion string    `json:"text_version,omitempty"`
	GrantedAt   time.Time `json:"granted_at,omitzero"`
	WithdrawnAt time.Time `json:"withdrawn_at,omitzero"`
	DeliveredAt time.Time `json:"delivered_at,omitzero"`
}

// Active reports whether the consent is granted and not withdrawn since.
func (c Consent) Active() bool {
	if c.GrantedAt.IsZero() {
		return false
	}
	return c.WithdrawnAt.IsZero() || c.WithdrawnAt.Before(c.GrantedAt)
}

// Covers reports whether the consent is active and includes field.
func (c Consent) Covers(field string) bool {
	return c.Active() && slices.Contains(c.Fields, field)
}

// Dismissal records an enrichment value an owner dismissed, so the same
// value is not suggested again.
type Dismissal struct {
	Field string    `json:"field"`
	Value string    `json:"value"`
	At    time.Time `json:"at,omitzero"`
	By    string    `json:"by,omitempty"`
}

// Person is everything the store holds about one person.
type Person struct {
	ID         string      `json:"id"`
	Status     string      `json:"status"`
	CreatedAt  time.Time   `json:"created_at,omitzero"`
	UpdatedAt  time.Time   `json:"updated_at,omitzero"`
	Identities []Identity  `json:"identities,omitempty"`
	Facts      []Fact      `json:"facts,omitempty"`
	Consents   []Consent   `json:"consents,omitempty"`
	Dismissed  []Dismissal `json:"dismissed,omitempty"`
}

// ViewOptions tunes EffectiveView.
type ViewOptions struct {
	// ExcludeSuggested leaves out enrichment values nobody accepted.
	ExcludeSuggested bool
}

// Effective returns the effective fact for field: the one at the highest
// rank present, suggestions included. A returned fact with an empty Value is
// an explicit clear. ok is false when no rank holds the field.
func (p Person) Effective(field string) (Fact, bool) {
	return p.effective(field, ViewOptions{})
}

func (p Person) effective(field string, opts ViewOptions) (Fact, bool) {
	var best Fact
	found := false
	for _, f := range p.Facts {
		if f.Field != field {
			continue
		}
		if opts.ExcludeSuggested && f.Suggested {
			continue
		}
		if !found || f.Rank.Outranks(best.Rank) {
			best, found = f, true
		}
	}
	return best, found
}

// EffectiveView returns the effective fact of every field the person has.
func (p Person) EffectiveView(opts ViewOptions) map[string]Fact {
	out := map[string]Fact{}
	for _, f := range p.Facts {
		if _, done := out[f.Field]; done {
			continue
		}
		if e, ok := p.effective(f.Field, opts); ok {
			out[f.Field] = e
		}
	}
	return out
}

// FactAt returns the fact for field at exactly rank.
func (p Person) FactAt(field string, rank Rank) (Fact, bool) {
	i := p.factIndex(field, rank)
	if i < 0 {
		return Fact{}, false
	}
	return p.Facts[i], true
}

// Consent returns the consent record for scope.
func (p Person) Consent(scope string) (Consent, bool) {
	for _, c := range p.Consents {
		if c.Scope == scope {
			return c, true
		}
	}
	return Consent{}, false
}

// HasIdentity reports whether the person holds the identity. Email kinds
// match each other.
func (p Person) HasIdentity(ref IdentityRef) bool {
	_, ok := p.identity(ref)
	return ok
}

func (p Person) identity(ref IdentityRef) (Identity, bool) {
	cls, v := identityClass(ref.Kind), normalizeValue(ref.Kind, ref.Value)
	for _, id := range p.Identities {
		if identityClass(id.Kind) == cls && normalizeValue(id.Kind, id.Value) == v {
			return id, true
		}
	}
	return Identity{}, false
}

func (p Person) factIndex(field string, rank Rank) int {
	for i, f := range p.Facts {
		if f.Field == field && f.Rank == rank {
			return i
		}
	}
	return -1
}

func (p Person) dismissed(field, value string) bool {
	nv := normalizeFactValue(value)
	for _, d := range p.Dismissed {
		if d.Field == field && normalizeFactValue(d.Value) == nv {
			return true
		}
	}
	return false
}

// factShareable reports whether a stored fact may leave the deployment.
func (p Person) factShareable(f Fact) bool {
	if f.Locality != LocalityShareable || slices.Contains(alwaysLocalFields, f.Field) {
		return false
	}
	for _, ref := range f.DerivedFrom {
		id, ok := p.identity(ref)
		if !ok || id.Locality != LocalityShareable {
			return false
		}
	}
	return true
}

// identityClass groups kinds that hold the same namespace of values, so an
// address forgotten as git_email is also forgotten as email.
func identityClass(k Kind) string {
	if k.IsEmail() {
		return string(KindEmail)
	}
	return string(k)
}

// normalizeValue folds a value for matching: logins and emails are case
// insensitive, and a leading "@" on a login is dropped.
func normalizeValue(k Kind, v string) string {
	v = strings.TrimSpace(v)
	switch k {
	case KindGitHub, KindGHE:
		return strings.ToLower(strings.TrimPrefix(v, "@"))
	case KindEmail, KindGitEmail:
		return strings.ToLower(v)
	default:
		return v
	}
}

func normalizeFactValue(v string) string {
	return strings.ToLower(strings.Join(strings.Fields(v), " "))
}

func checkText(what, s string, maxBytes int, required bool) error {
	if required && strings.TrimSpace(s) == "" {
		return invalid("%s is required", what)
	}
	if len(s) > maxBytes {
		return invalid("%s is longer than %d bytes", what, maxBytes)
	}
	if !utf8.ValidString(s) {
		return invalid("%s is not valid UTF-8", what)
	}
	for _, r := range s {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return invalid("%s holds a control character", what)
		}
	}
	return nil
}

func checkID(id string) error { return checkText("person id", id, maxIDBytes, true) }

func checkRef(ref IdentityRef) error {
	if !ref.Kind.Valid() {
		return invalid("unknown identity kind %q", ref.Kind)
	}
	if err := checkText("identity value", ref.Value, maxIdentityBytes, true); err != nil {
		return err
	}
	if strings.ContainsAny(ref.Value, "\n\t") {
		return invalid("identity value holds whitespace control characters")
	}
	if ref.Kind.IsEmail() && !strings.Contains(ref.Value, "@") {
		return invalid("email identity has no @")
	}
	return nil
}
