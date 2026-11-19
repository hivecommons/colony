// Package resolver defines the provider contract for contributor enrichment
// and a Resolver that merges several providers into one answer.
//
// Every fact in a Result carries the name of the provider that produced it,
// so a consumer can show its origin and let a person accept or dismiss it.
package resolver

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Confidence says how strongly a provider believes a fact applies to the
// queried person.
type Confidence string

const (
	// ConfidenceHigh is a match on a curated identifier, such as a GitHub
	// login listed in a maintained affiliation file.
	ConfidenceHigh Confidence = "high"
	// ConfidenceMedium is a match on a weaker identifier, such as an email
	// address that a curated source lists on a "best effort" basis.
	ConfidenceMedium Confidence = "medium"
	// ConfidenceLow is a heuristic match.
	ConfidenceLow Confidence = "low"
)

// Query identifies the person to look up. At least one field must be set.
// Providers that contact a remote service per person must only send
// GitHubLogin; Emails are for local matching.
type Query struct {
	GitHubLogin string   `json:"github_login,omitempty"`
	Emails      []string `json:"emails,omitempty"`
}

// Empty reports whether the query has no identifier at all.
func (q Query) Empty() bool {
	if strings.TrimSpace(q.GitHubLogin) != "" {
		return false
	}
	for _, e := range q.Emails {
		if strings.TrimSpace(e) != "" {
			return false
		}
	}
	return true
}

// ErrEmptyQuery is returned when a Query has no identifier.
var ErrEmptyQuery = errors.New("resolver: query has no GitHub login or email")

// Affiliation is one employment (or "Independent") period. A zero From means
// "since before the source's records start"; a zero Until means "still
// current" as far as the source knows. Until is exclusive.
type Affiliation struct {
	Org        string     `json:"org"`
	From       time.Time  `json:"from,omitzero"`
	Until      time.Time  `json:"until,omitzero"`
	Source     string     `json:"source"`
	Confidence Confidence `json:"confidence"`
	// MatchedBy names the identifier kind that matched ("github_login" or
	// "email"), never the identifier value.
	MatchedBy string `json:"matched_by,omitempty"`
}

// CurrentAt reports whether the affiliation covers instant t.
func (a Affiliation) CurrentAt(t time.Time) bool {
	if !a.From.IsZero() && t.Before(a.From) {
		return false
	}
	return a.Until.IsZero() || t.Before(a.Until)
}

// Activity is contribution activity in one project over one period.
type Activity struct {
	Project       string `json:"project"`
	Contributions int    `json:"contributions"`
	// Period is a provider-defined label such as "all time" or "last year".
	Period string `json:"period,omitempty"`
	URL    string `json:"url,omitempty"`
	Source string `json:"source"`
}

// ProviderError records a provider that failed during a Resolver lookup.
type ProviderError struct {
	Source string `json:"source"`
	Error  string `json:"error"`
}

// Result is what one provider, or the merged Resolver, knows about a person.
type Result struct {
	Affiliations []Affiliation `json:"affiliations,omitempty"`
	Activity     []Activity    `json:"activity,omitempty"`
	// Sources lists the providers that contributed at least one fact.
	Sources []string `json:"sources,omitempty"`
	// Errors lists providers that failed; the rest of the Result is still
	// valid. Only set by Resolver.
	Errors []ProviderError `json:"errors,omitempty"`
}

// Empty reports whether the result carries no facts.
func (r Result) Empty() bool {
	return len(r.Affiliations) == 0 && len(r.Activity) == 0
}

// CurrentAffiliations returns the affiliations that cover instant t, in
// precedence order.
func (r Result) CurrentAffiliations(t time.Time) []Affiliation {
	var out []Affiliation
	for _, a := range r.Affiliations {
		if a.CurrentAt(t) {
			out = append(out, a)
		}
	}
	return out
}

// Provider is one enrichment source. A provider that knows nothing about the
// person returns an empty Result and a nil error.
type Provider interface {
	// Name is the stable source name stamped on every fact, e.g. "gitdm".
	Name() string
	Lookup(ctx context.Context, q Query) (Result, error)
}

// Resolver queries providers in precedence order and merges their results.
type Resolver struct {
	providers []Provider
}

// New returns a Resolver over providers. Earlier providers take precedence:
// when two providers report the same affiliation period or the same project
// activity, the earlier provider's fact is kept.
func New(providers ...Provider) *Resolver {
	return &Resolver{providers: slices.Clone(providers)}
}

// Lookup asks every provider about q and merges the answers. A failing
// provider is recorded in Result.Errors and does not hide the others. The
// returned error is non-nil only for an empty query, a cancelled context, or
// when every provider failed.
func (r *Resolver) Lookup(ctx context.Context, q Query) (Result, error) {
	if q.Empty() {
		return Result{}, ErrEmptyQuery
	}
	var merged Result
	seenAff := map[string]bool{}
	seenAct := map[string]bool{}
	var errs []error
	for _, p := range r.providers {
		if err := ctx.Err(); err != nil {
			return merged, err
		}
		res, err := p.Lookup(ctx, q)
		if err != nil {
			merged.Errors = append(merged.Errors, ProviderError{Source: p.Name(), Error: err.Error()})
			errs = append(errs, fmt.Errorf("%s: %w", p.Name(), err))
			continue
		}
		contributed := false
		for _, a := range res.Affiliations {
			if a.Source == "" {
				a.Source = p.Name()
			}
			key := affiliationKey(a)
			if seenAff[key] {
				continue
			}
			seenAff[key] = true
			merged.Affiliations = append(merged.Affiliations, a)
			contributed = true
		}
		for _, a := range res.Activity {
			if a.Source == "" {
				a.Source = p.Name()
			}
			key := strings.ToLower(a.Project) + "\x00" + a.Period
			if seenAct[key] {
				continue
			}
			seenAct[key] = true
			merged.Activity = append(merged.Activity, a)
			contributed = true
		}
		if contributed {
			merged.Sources = append(merged.Sources, p.Name())
		}
	}
	if len(r.providers) > 0 && len(errs) == len(r.providers) {
		return merged, errors.Join(errs...)
	}
	return merged, nil
}

func affiliationKey(a Affiliation) string {
	return NormalizeOrg(a.Org) + "\x00" + a.From.Format(time.DateOnly) + "\x00" + a.Until.Format(time.DateOnly)
}

// NormalizeOrg folds an organisation name for comparison: case, surrounding
// space and runs of inner space are ignored.
func NormalizeOrg(org string) string {
	return strings.ToLower(strings.Join(strings.Fields(org), " "))
}
