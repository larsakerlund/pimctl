// Tests for the per-run context: the scope-name table that lets a message name
// a scope the way the tables do. Context resolution and session opening are
// covered by context_test.go and session_test.go.

package cli

import "testing"

// TestScopeNamesLabelStaysCorrectWhenMoreNamesArrive: the labeler is built once
// and kept, so labelling N scopes is one walk of the map rather than N. The
// risk that buys is staleness — one more name can make a previously unique
// display name ambiguous — so learning invalidates it.
func TestScopeNamesLabelStaysCorrectWhenMoreNamesArrive(t *testing.T) {
	const (
		prod = "/providers/Microsoft.Management/managementGroups/contoso-prod"
		test = "/providers/Microsoft.Management/managementGroups/contoso-test"
		sub  = "/subscriptions/22222222-0000-0000-0000-000000000002"
	)
	var names scopeNames

	names.learn(sub, "Contoso QA")
	if got := names.label(sub); got != "Contoso QA" {
		t.Errorf("a lone subscription should not grow an id: %q", got)
	}

	// Learning a second scope with the same display name must change the
	// answer for the first — a cached labeler that did not notice would print
	// two different subscriptions identically.
	names.learn(prod, "Contoso QA")
	for _, id := range []string{sub, prod} {
		want := "Contoso QA (" + scopeLeaf(id) + ")"
		if got := names.label(id); got != want {
			t.Errorf("label(%s) = %q, want %q once the name is shared", scopeLeaf(id), got, want)
		}
	}

	// Repeated calls are stable, which is the point of keeping the labeler.
	first := names.label(prod)
	for range 3 {
		if got := names.label(prod); got != first {
			t.Errorf("label is not stable across calls: %q then %q", first, got)
		}
	}

	// An id nobody named falls back to its leaf rather than an empty string.
	if got := names.label(test); got != scopeLeaf(test) {
		t.Errorf("unknown scope labelled %q, want its leaf %q", got, scopeLeaf(test))
	}
}
