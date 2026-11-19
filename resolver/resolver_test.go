package resolver

import (
	"context"
	"errors"
	"testing"
	"time"
)

type stubProvider struct {
	name string
	res  Result
	err  error
	got  []Query
}

func (s *stubProvider) Name() string { return s.name }

func (s *stubProvider) Lookup(_ context.Context, q Query) (Result, error) {
	s.got = append(s.got, q)
	return s.res, s.err
}

func day(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestQueryEmpty(t *testing.T) {
	cases := []struct {
		q    Query
		want bool
	}{
		{Query{}, true},
		{Query{GitHubLogin: "  "}, true},
		{Query{Emails: []string{"", " "}}, true},
		{Query{GitHubLogin: "dims"}, false},
		{Query{Emails: []string{"a@b.c"}}, false},
	}
	for _, c := range cases {
		if got := c.q.Empty(); got != c.want {
			t.Errorf("%+v.Empty() = %v, want %v", c.q, got, c.want)
		}
	}
}

func TestAffiliationCurrentAt(t *testing.T) {
	a := Affiliation{Org: "X", From: day("2020-01-01"), Until: day("2021-01-01")}
	if a.CurrentAt(day("2019-12-31")) {
		t.Error("before From must not be current")
	}
	if !a.CurrentAt(day("2020-01-01")) {
		t.Error("From is inclusive")
	}
	if a.CurrentAt(day("2021-01-01")) {
		t.Error("Until is exclusive")
	}
	open := Affiliation{Org: "Y"}
	if !open.CurrentAt(day("1999-01-01")) {
		t.Error("open-ended affiliation is always current")
	}
}

func TestResolverMergesByPrecedence(t *testing.T) {
	first := &stubProvider{name: "first", res: Result{
		Affiliations: []Affiliation{{Org: "Google LLC", From: day("2018-08-01")}},
		Activity:     []Activity{{Project: "Kubernetes", Contributions: 10, Period: "all"}},
	}}
	second := &stubProvider{name: "second", res: Result{
		Affiliations: []Affiliation{
			{Org: "google  llc", From: day("2018-08-01")}, // same fact, lower precedence
			{Org: "Red Hat Inc.", Until: day("2018-08-01"), Source: "explicit"},
		},
		Activity: []Activity{
			{Project: "kubernetes", Contributions: 99, Period: "all"},
			{Project: "Helm", Contributions: 2, Period: "all"},
		},
	}}
	silent := &stubProvider{name: "silent"}
	r := New(first, second, silent)
	q := Query{GitHubLogin: "liggitt"}
	res, err := r.Lookup(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Affiliations) != 2 {
		t.Fatalf("affiliations = %+v", res.Affiliations)
	}
	if res.Affiliations[0].Source != "first" || res.Affiliations[0].Org != "Google LLC" {
		t.Errorf("precedence lost: %+v", res.Affiliations[0])
	}
	if res.Affiliations[1].Source != "explicit" {
		t.Errorf("explicit Source must be kept: %+v", res.Affiliations[1])
	}
	if len(res.Activity) != 2 || res.Activity[0].Contributions != 10 || res.Activity[1].Source != "second" {
		t.Errorf("activity = %+v", res.Activity)
	}
	if got := res.Sources; len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Errorf("sources = %v", got)
	}
	if len(silent.got) != 1 || silent.got[0].GitHubLogin != "liggitt" {
		t.Errorf("every provider must be asked: %+v", silent.got)
	}
}

func TestResolverPartialFailure(t *testing.T) {
	bad := &stubProvider{name: "bad", err: errors.New("boom")}
	good := &stubProvider{name: "good", res: Result{Affiliations: []Affiliation{{Org: "CNCF"}}}}
	res, err := New(bad, good).Lookup(context.Background(), Query{GitHubLogin: "x"})
	if err != nil {
		t.Fatalf("partial failure must not fail the lookup: %v", err)
	}
	if len(res.Errors) != 1 || res.Errors[0].Source != "bad" || res.Errors[0].Error != "boom" {
		t.Errorf("errors = %+v", res.Errors)
	}
	if len(res.Affiliations) != 1 || res.Affiliations[0].Source != "good" {
		t.Errorf("affiliations = %+v", res.Affiliations)
	}
}

func TestResolverAllFail(t *testing.T) {
	cause := errors.New("down")
	_, err := New(&stubProvider{name: "a", err: cause}, &stubProvider{name: "b", err: cause}).
		Lookup(context.Background(), Query{GitHubLogin: "x"})
	if !errors.Is(err, cause) {
		t.Fatalf("want joined cause, got %v", err)
	}
}

func TestResolverEmptyQueryAndCancel(t *testing.T) {
	p := &stubProvider{name: "p"}
	if _, err := New(p).Lookup(context.Background(), Query{}); !errors.Is(err, ErrEmptyQuery) {
		t.Fatalf("want ErrEmptyQuery, got %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New(p).Lookup(ctx, Query{GitHubLogin: "x"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if len(p.got) != 0 {
		t.Error("no provider may be called for an empty query or a cancelled context")
	}
}

func TestResultHelpers(t *testing.T) {
	r := Result{Affiliations: []Affiliation{
		{Org: "Old", Until: day("2020-01-01")},
		{Org: "New", From: day("2020-01-01")},
	}}
	if r.Empty() {
		t.Error("not empty")
	}
	cur := r.CurrentAffiliations(day("2024-05-05"))
	if len(cur) != 1 || cur[0].Org != "New" {
		t.Errorf("current = %+v", cur)
	}
	if !(Result{}).Empty() {
		t.Error("zero result is empty")
	}
	if got := NormalizeOrg("  Red   Hat\tInc. "); got != "red hat inc." {
		t.Errorf("NormalizeOrg = %q", got)
	}
}
