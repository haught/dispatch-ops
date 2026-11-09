package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseAllowedJobNamespaces(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    map[string][]string
		wantErr string
	}{
		{in: "", want: map[string][]string{}},
		{in: "jobs-a=team-a", want: map[string][]string{"jobs-a": {"team-a"}}},
		{in: " jobs-a = team-a , jobs-b=team-c|team-b|team-c ,", want: map[string][]string{
			"jobs-a": {"team-a"}, "jobs-b": {"team-b", "team-c"},
		}},
		// A bare job namespace would be open to every namespace: refused.
		{in: "jobs-a", wantErr: `"jobs-a" doesn't say which namespaces`},
		{in: "jobs-a=team-a,jobs-b", wantErr: `"jobs-b" doesn't say which namespaces`},
		{in: "jobs-a=", wantErr: "empty namespace"},
		{in: "jobs-a=team-a|", wantErr: "empty namespace"},
		{in: "=team-a", wantErr: "is not a namespace"},
		{in: "Jobs=team-a", wantErr: "is not a namespace"},
		{in: "jobs=team_a", wantErr: "is not a namespace"},
		{in: "jobs=team-a,jobs=team-b", wantErr: "listed twice"},
	} {
		got, err := parseAllowedJobNamespaces(tc.in)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%q: error %v, want %q", tc.in, err, tc.wantErr)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
}

func TestCheckAllowedJobNamespaces(t *testing.T) {
	allowed := map[string][]string{"jobs-a": {"team-a"}, "jobs-b": {"team-b", "team-c"}}
	for _, tc := range []struct {
		name    string
		watched []string
		wantErr string
	}{
		{name: "all namespaces"},
		{name: "watched", watched: []string{"team-a", "team-b", "team-c"}},
		// R2-H2 F1: a namespace that hosts DispatchOps can't be a job
		// namespace.
		{name: "overlap", watched: []string{"team-a", "team-b", "team-c", "jobs-b"}, wantErr: "jobs-b is also in --watch-namespaces"},
		{name: "source not watched", watched: []string{"team-a", "team-b"}, wantErr: "team-c is not in --watch-namespaces"},
	} {
		err := checkAllowedJobNamespaces(allowed, tc.watched)
		if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
			t.Errorf("%s: error %v, want %q", tc.name, err, tc.wantErr)
		}
	}
	// In cluster mode, a job namespace may name its own (pointless, not
	// harmful); watched, it overlaps.
	if err := checkAllowedJobNamespaces(map[string][]string{"jobs": {"jobs"}}, nil); err != nil {
		t.Error(err)
	}
	if err := checkAllowedJobNamespaces(map[string][]string{"jobs": {"jobs"}}, []string{"jobs"}); err == nil {
		t.Error("overlap accepted")
	}
}
