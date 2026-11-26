package store

import (
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/colony/resolver"
)

// matchedByLogin and matchedByEmail are the resolver.Affiliation.MatchedBy
// values colony providers report.
const (
	matchedByLogin = "github_login"
	matchedByEmail = "email"
)

// FactsFromResult turns a resolver answer for q into enrichment facts that
// SetFact accepts, with each provider's source and confidence intact:
//
//   - the first affiliation current at instant at becomes a suggested
//     FieldAffiliation fact;
//   - each activity becomes a suggested FieldActivityPrefix+project fact
//     whose value is the contribution count.
//
// Every fact records the query identities it was derived from, so a value
// matched through a local_only email stays local_only.
func FactsFromResult(res resolver.Result, q resolver.Query, at time.Time) []Fact {
	var out []Fact
	login := strings.TrimPrefix(strings.TrimSpace(q.GitHubLogin), "@")
	if cur := res.CurrentAffiliations(at); len(cur) > 0 {
		a := cur[0]
		out = append(out, Fact{
			Field:       FieldAffiliation,
			Value:       a.Org,
			Rank:        RankEnrichment,
			Source:      colonySource(a.Source),
			Confidence:  a.Confidence,
			Locality:    LocalityShareable,
			From:        a.From,
			Until:       a.Until,
			At:          at,
			Suggested:   true,
			DerivedFrom: derivedFrom(a.MatchedBy, login, q.Emails),
		})
	}
	for _, act := range res.Activity {
		if login == "" || strings.TrimSpace(act.Project) == "" {
			continue
		}
		out = append(out, Fact{
			Field:       FieldActivityPrefix + activityKey(act.Project),
			Value:       strconv.Itoa(act.Contributions),
			Rank:        RankEnrichment,
			Source:      colonySource(act.Source),
			Locality:    LocalityShareable,
			At:          at,
			Suggested:   true,
			DerivedFrom: []IdentityRef{{Kind: KindGitHub, Value: login}},
		})
	}
	return out
}

// derivedFrom names the identities a match used. When the provider did not
// say, every queried identity counts, so a private email can never be
// hidden behind an unlabelled match.
func derivedFrom(matchedBy, login string, emails []string) []IdentityRef {
	var refs []IdentityRef
	if login != "" && matchedBy != matchedByEmail {
		refs = append(refs, IdentityRef{Kind: KindGitHub, Value: login})
	}
	if matchedBy != matchedByLogin {
		for _, e := range emails {
			if e = strings.TrimSpace(e); e != "" {
				refs = append(refs, IdentityRef{Kind: KindEmail, Value: e})
			}
		}
	}
	return refs
}

func colonySource(src string) string {
	if src == "" || strings.HasPrefix(src, ColonySourcePrefix) {
		return src
	}
	return ColonySourcePrefix + src
}

func activityKey(project string) string {
	return strings.ReplaceAll(strings.ToLower(strings.Join(strings.Fields(project), " ")), " ", "_")
}
