package server

import (
	"testing"

	"github.com/pufferfs/pufferfs/internal/auth"
)

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

func TestNormalizeExplicitAPIKeyScopesRejectsUnknown(t *testing.T) {
	if _, err := normalizeExplicitAPIKeyScopes([]string{"query", "billing:write"}); err == nil {
		t.Fatal("normalizeExplicitAPIKeyScopes accepted unsupported scope")
	}
}

func TestRoleManagementRules(t *testing.T) {
	if !canAssignRole(auth.RoleOwner, auth.RoleAdmin) {
		t.Fatal("owner should be able to assign admin")
	}
	if !canAssignRole(auth.RoleAdmin, auth.RoleEditor) {
		t.Fatal("admin should be able to assign editor")
	}
	if canAssignRole(auth.RoleAdmin, auth.RoleOwner) {
		t.Fatal("admin should not be able to assign owner")
	}
	if canManageMemberRole(auth.RoleAdmin, auth.RoleAdmin) {
		t.Fatal("admin should not be able to manage another admin")
	}
}
