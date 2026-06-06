package server

import "testing"

func TestNormalizeExplicitAPIKeyScopes(t *testing.T) {
	got, err := normalizeExplicitAPIKeyScopes([]string{" query ", "", "sync", "query"})
	if err != nil {
		t.Fatalf("normalizeExplicitAPIKeyScopes returned error: %v", err)
	}
	want := []string{"query", "sync"}
	if len(got) != len(want) {
		t.Fatalf("scopes = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("scopes = %#v, want %#v", got, want)
		}
	}
}

func TestNormalizeExplicitAPIKeyScopesRejectsEmpty(t *testing.T) {
	for _, scopes := range [][]string{nil, []string{}, []string{"", "  "}} {
		if _, err := normalizeExplicitAPIKeyScopes(scopes); err == nil {
			t.Fatalf("normalizeExplicitAPIKeyScopes(%#v) accepted empty scopes", scopes)
		}
	}
}
