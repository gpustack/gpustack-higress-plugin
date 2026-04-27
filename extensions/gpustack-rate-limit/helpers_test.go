package main

import (
	"testing"
	"time"
)

// Pointer helpers for tabular tests.
func intPtr(v int) *int       { return &v }
func strPtr(v string) *string { return &v }

// mustCompile compiles a MatchRule or fails the test.
func mustCompile(t *testing.T, r MatchRule) MatchRule {
	t.Helper()
	if err := r.Compile(); err != nil {
		t.Fatalf("unexpected MatchRule.Compile error: %v", err)
	}
	return r
}

// mustCompileQuota compiles a QuotaSpec against loc (UTC if nil) or fails the test.
func mustCompileQuota(t *testing.T, spec QuotaSpec, loc *time.Location) QuotaSpec {
	t.Helper()
	if loc == nil {
		loc = time.UTC
	}
	if err := spec.Compile(loc); err != nil {
		t.Fatalf("unexpected QuotaSpec.Compile error: %v", err)
	}
	return spec
}

// mustLoadLocation loads an IANA timezone or fails the test.
func mustLoadLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return loc
}
