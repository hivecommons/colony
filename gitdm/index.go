package gitdm

import (
	"strings"

	"github.com/hivecommons/colony/resolver"
)

// SourceName is stamped on every fact this provider returns.
const SourceName = "gitdm"

const (
	matchedByLogin = "github_login"
	matchedByEmail = "email"
)

// maxAliasHops bounds alias expansion so a malformed alias cycle cannot loop.
const maxAliasHops = 8

// Index is an in-memory, read-only lookup over parsed gitdm data. It is
// safe for concurrent use once built.
type Index struct {
	byLogin map[string][]int
	byEmail map[string][]int
	aliases map[string][]string
	devs    []Developer
}

// NewIndex builds an Index. Logins and emails match case-insensitively. A
// login listed more than once (gitdm has a few case variants) maps to every
// entry.
func NewIndex(devs []Developer, aliases [][2]string) *Index {
	ix := &Index{
		byLogin: make(map[string][]int, len(devs)),
		byEmail: make(map[string][]int, len(devs)),
		aliases: make(map[string][]string, len(aliases)),
		devs:    devs,
	}
	for i, d := range devs {
		login := strings.ToLower(d.Login)
		ix.byLogin[login] = append(ix.byLogin[login], i)
		for _, e := range d.Emails {
			ix.byEmail[e] = append(ix.byEmail[e], i)
		}
	}
	for _, pair := range aliases {
		ix.aliases[pair[0]] = append(ix.aliases[pair[0]], pair[1])
		ix.aliases[pair[1]] = append(ix.aliases[pair[1]], pair[0])
	}
	return ix
}

// Developers returns how many developer entries the index holds.
func (ix *Index) Developers() int { return len(ix.devs) }

// Lookup matches q against the index. A GitHub login match is high
// confidence; an email match (gitdm calls its emails "best effort") is
// medium. When both identify the same entry, the login match is reported.
func (ix *Index) Lookup(q resolver.Query) resolver.Result {
	var res resolver.Result
	seen := map[int]bool{}
	add := func(idx int, conf resolver.Confidence, by string) {
		if seen[idx] {
			return
		}
		seen[idx] = true
		for _, p := range ix.devs[idx].Affiliations {
			res.Affiliations = append(res.Affiliations, resolver.Affiliation{
				Org:        p.Org,
				From:       p.From,
				Until:      p.Until,
				Source:     SourceName,
				Confidence: conf,
				MatchedBy:  by,
			})
		}
	}
	if login := strings.ToLower(strings.TrimSpace(q.GitHubLogin)); login != "" {
		for _, idx := range ix.byLogin[login] {
			add(idx, resolver.ConfidenceHigh, matchedByLogin)
		}
	}
	for _, email := range ix.expandEmails(q.Emails) {
		for _, idx := range ix.byEmail[email] {
			add(idx, resolver.ConfidenceMedium, matchedByEmail)
		}
	}
	res.Affiliations = dedupe(res.Affiliations)
	if len(res.Affiliations) > 0 {
		res.Sources = []string{SourceName}
	}
	return res
}

// expandEmails normalises the query emails and adds every address the
// aliases file declares equivalent.
func (ix *Index) expandEmails(emails []string) []string {
	seen := map[string]bool{}
	var out, frontier []string
	for _, e := range emails {
		e = strings.ToLower(strings.TrimSpace(e))
		if e != "" && !seen[e] {
			seen[e] = true
			out = append(out, e)
			frontier = append(frontier, e)
		}
	}
	for hop := 0; hop < maxAliasHops && len(frontier) > 0; hop++ {
		var next []string
		for _, e := range frontier {
			for _, alt := range ix.aliases[e] {
				if !seen[alt] {
					seen[alt] = true
					out = append(out, alt)
					next = append(next, alt)
				}
			}
		}
		frontier = next
	}
	return out
}

func dedupe(in []resolver.Affiliation) []resolver.Affiliation {
	seen := map[string]bool{}
	out := in[:0]
	for _, a := range in {
		key := resolver.NormalizeOrg(a.Org) + "\x00" + a.From.String() + "\x00" + a.Until.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, a)
	}
	return out
}
