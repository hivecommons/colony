package gitdm

import (
	"os"
	"strings"
	"testing"
	"time"
)

func day(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

func parseFixture(t *testing.T, name string) []Developer {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	devs, err := ParseAffiliations(f)
	if err != nil {
		t.Fatal(err)
	}
	return devs
}

func find(devs []Developer, login string) *Developer {
	for i := range devs {
		if devs[i].Login == login {
			return &devs[i]
		}
	}
	return nil
}

func TestParseRealFixture(t *testing.T) {
	devs := parseFixture(t, "developers_affiliations1.txt")
	if len(devs) != 3 {
		t.Fatalf("got %d developers, want 3", len(devs))
	}
	dims := find(devs, "dims")
	if dims == nil {
		t.Fatal("dims missing")
	}
	wantEmails := []string{"davanum@gmail.com", "dims@linux.vnet.ibm.com", "dims@users.noreply.github.com"}
	if strings.Join(dims.Emails, ",") != strings.Join(wantEmails, ",") {
		t.Errorf("emails = %v", dims.Emails)
	}
	if len(dims.Affiliations) != 7 {
		t.Fatalf("dims affiliations = %+v", dims.Affiliations)
	}
	first, last := dims.Affiliations[0], dims.Affiliations[6]
	if first.Org != "International Business Machines Corporation" || !first.From.IsZero() || !first.Until.Equal(day("2015-01-14")) {
		t.Errorf("first = %+v", first)
	}
	mid := dims.Affiliations[3]
	if mid.Org != "VMware Inc." || !mid.From.Equal(day("2019-06-14")) || !mid.Until.Equal(day("2022-09-23")) {
		t.Errorf("mid = %+v", mid)
	}
	if last.Org != "NVIDIA Corporation" || !last.From.Equal(day("2025-07-11")) || !last.Until.IsZero() {
		t.Errorf("last = %+v", last)
	}
	thockin := find(devs, "thockin")
	if thockin == nil || len(thockin.Affiliations) != 1 || thockin.Affiliations[0].Org != "Google LLC" ||
		!thockin.Affiliations[0].From.IsZero() || !thockin.Affiliations[0].Until.IsZero() {
		t.Errorf("thockin = %+v", thockin)
	}
}

func TestParseOrgContainingFrom(t *testing.T) {
	devs := parseFixture(t, "developers_affiliations2.txt")
	f := find(devs, "FlorianOtel")
	if f == nil {
		t.Fatal("FlorianOtel missing")
	}
	got := f.Affiliations[2]
	if got.Org != "Nuage Networks from Nokia" || !got.From.Equal(day("2014-12-01")) || !got.Until.Equal(day("2018-04-01")) {
		t.Errorf("Nuage line = %+v", got)
	}
}

func TestParseEdgeCases(t *testing.T) {
	in := "\ufeff# header\r\n" +
		"\tOrphan affiliation\n" +
		"star*: star!example.com, (none), word, bad at example.com, x!nodot\n" +
		"\tNotFound\n" +
		"\t(Unknown)\n" +
		"\tAcme from not-a-date\n" +
		"  Spaces Corp until 2020-01-01\r\n" +
		"no colon line\n" +
		"\tDropped because its developer line was invalid\n" +
		"bad login: x!example.com\n" +
		"plain: plain!EXAMPLE.org\n" +
		"\n"
	devs, err := ParseAffiliations(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 2 {
		t.Fatalf("devs = %+v", devs)
	}
	star := devs[0]
	if star.Login != "star" {
		t.Errorf("trailing * must be dropped, got %q", star.Login)
	}
	if len(star.Emails) != 1 || star.Emails[0] != "star@example.com" {
		t.Errorf("emails = %v", star.Emails)
	}
	if len(star.Affiliations) != 2 {
		t.Fatalf("affiliations = %+v", star.Affiliations)
	}
	if star.Affiliations[0].Org != "Acme from not-a-date" || !star.Affiliations[0].From.IsZero() {
		t.Errorf("undated org must stay whole: %+v", star.Affiliations[0])
	}
	if star.Affiliations[1].Org != "Spaces Corp" || !star.Affiliations[1].Until.Equal(day("2020-01-01")) {
		t.Errorf("space-indented line = %+v", star.Affiliations[1])
	}
	if devs[1].Emails[0] != "plain@example.org" {
		t.Errorf("emails are lower-cased: %v", devs[1].Emails)
	}
}

func TestParseAliases(t *testing.T) {
	f, err := os.Open("testdata/aliases")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	pairs, err := ParseAliases(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 1 || pairs[0] != [2]string{"brendandburns@users.noreply.github.com", "bburns@google.com"} {
		t.Errorf("pairs = %v", pairs)
	}
	more, err := ParseAliases(strings.NewReader("a!x.org a!x.org\none!x.org\nthree a b\n(none) b!x.org\n"))
	if err != nil || len(more) != 0 {
		t.Errorf("malformed alias lines must be skipped: %v %v", more, err)
	}
}

func TestParseLineTooLong(t *testing.T) {
	in := "login: " + strings.Repeat("x", maxLineBytes+1) + "\n"
	if _, err := ParseAffiliations(strings.NewReader(in)); err == nil {
		t.Error("over-long line must fail")
	}
	if _, err := ParseAliases(strings.NewReader(in)); err == nil {
		t.Error("over-long alias line must fail")
	}
}
