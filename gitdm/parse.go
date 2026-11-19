// Package gitdm is the colony provider for cncf/gitdm affiliation data.
//
// It downloads the public developers_affiliations*.txt and email alias files
// from the cncf/gitdm repository (the same files for every caller), caches
// them, and matches GitHub logins and email addresses locally. No per-person
// data is ever sent anywhere.
package gitdm

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"time"
)

// Period is one line of a developer's affiliation history.
type Period struct {
	Org   string
	From  time.Time
	Until time.Time
}

// Developer is one entry of a developers_affiliations file.
type Developer struct {
	Login        string
	Emails       []string
	Affiliations []Period
}

// maxLineBytes bounds a single line; real lines are well under 4 KiB.
const maxLineBytes = 1 << 20

// emailAtMarker is the "@" substitute gitdm uses in published files.
const emailAtMarker = "!"

// placeholderOrgs are gitdm values that mean "no known affiliation"; they are
// dropped rather than reported as an organisation.
var placeholderOrgs = map[string]bool{
	"notfound":    true,
	"(notfound)":  true,
	"not found":   true,
	"(not found)": true,
	"(unknown)":   true,
	"unknown":     true,
	"?":           true,
}

// ParseAffiliations parses a cncf/gitdm developers_affiliations file.
//
// The format is:
//
//	# comment
//	login: email1!example.com, email2!example.org
//		Org A until 2020-01-01
//		Org B from 2020-01-01 until 2022-06-01
//		Org C from 2022-06-01
//
// A developer line is "login: emails", where each email uses "!" in place
// of "@" and a trailing "*" on the login (gitdm's marker for conflicting
// records) is dropped. Each following indented line is one affiliation:
// an organisation, optionally followed by " from YYYY-MM-DD" and/or
// " until YYYY-MM-DD". A line without dates is the only (current)
// affiliation. Organisation names may themselves contain " from ", as in
// "Nuage Networks from Nokia from 2014-12-01", so dates are only taken from
// the end of the line.
func ParseAffiliations(r io.Reader) ([]Developer, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), maxLineBytes)
	var out []Developer
	cur := -1
	lineNo := 0
	for sc.Scan() {
		lineNo++
		raw := strings.TrimRight(sc.Text(), "\r")
		if lineNo == 1 {
			raw = strings.TrimPrefix(raw, "\ufeff")
		}
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if raw[0] == '\t' || raw[0] == ' ' {
			if cur < 0 {
				continue // affiliation before any developer: ignore
			}
			p, ok := parsePeriod(trimmed)
			if ok {
				out[cur].Affiliations = append(out[cur].Affiliations, p)
			}
			continue
		}
		dev, ok := parseDeveloper(trimmed)
		if !ok {
			cur = -1
			continue
		}
		out = append(out, dev)
		cur = len(out) - 1
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("gitdm: line %d: %w", lineNo+1, err)
	}
	return out, nil
}

func parseDeveloper(line string) (Developer, bool) {
	login, emails, found := strings.Cut(line, ":")
	if !found {
		return Developer{}, false
	}
	login = strings.TrimSpace(strings.TrimRight(strings.TrimSpace(login), "*"))
	if login == "" || strings.ContainsAny(login, " \t") {
		return Developer{}, false
	}
	dev := Developer{Login: login}
	for _, e := range strings.Split(emails, ",") {
		if addr, ok := decodeEmail(e); ok {
			dev.Emails = append(dev.Emails, addr)
		}
	}
	return dev, true
}

// decodeEmail turns gitdm's "user!example.com" into "user@example.com".
// Values that are not an address ("(none)", bare words) are rejected.
func decodeEmail(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if strings.ContainsAny(s, " \t") {
		return "", false
	}
	local, domain, found := strings.Cut(s, emailAtMarker)
	if !found {
		local, domain, found = strings.Cut(s, "@")
	}
	if !found || local == "" || domain == "" || !strings.Contains(domain, ".") {
		return "", false
	}
	return strings.ToLower(local + "@" + domain), true
}

const (
	untilMarker = " until "
	fromMarker  = " from "
)

func parsePeriod(line string) (Period, bool) {
	var p Period
	rest := line
	if org, date, ok := cutDateSuffix(rest, untilMarker); ok {
		p.Until = date
		rest = org
	}
	if org, date, ok := cutDateSuffix(rest, fromMarker); ok {
		p.From = date
		rest = org
	}
	p.Org = strings.TrimSpace(rest)
	if p.Org == "" || placeholderOrgs[strings.ToLower(p.Org)] {
		return Period{}, false
	}
	return p, true
}

// cutDateSuffix splits "Org<marker>YYYY-MM-DD" at the last marker when what
// follows is exactly a date.
func cutDateSuffix(s, marker string) (string, time.Time, bool) {
	i := strings.LastIndex(s, marker)
	if i < 0 {
		return s, time.Time{}, false
	}
	t, err := time.Parse(time.DateOnly, strings.TrimSpace(s[i+len(marker):]))
	if err != nil {
		return s, time.Time{}, false
	}
	return s[:i], t, true
}

// ParseAliases parses a gitdm aliases file: one pair of equivalent email
// addresses per line ("alias canonical", "!" in place of "@"), "#" comments.
func ParseAliases(r io.Reader) ([][2]string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), maxLineBytes)
	var out [][2]string
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		a, okA := decodeEmail(fields[0])
		b, okB := decodeEmail(fields[1])
		if okA && okB && a != b {
			out = append(out, [2]string{a, b})
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("gitdm: aliases: %w", err)
	}
	return out, nil
}
