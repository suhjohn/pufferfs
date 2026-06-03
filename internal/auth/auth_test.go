package auth

import (
	"testing"
	"time"
)

func TestHashAPIKey(t *testing.T) {
	hash1 := HashAPIKey("pfs_test-key-123")
	hash2 := HashAPIKey("pfs_test-key-123")
	if hash1 != hash2 {
		t.Errorf("same input produced different hashes: %s vs %s", hash1, hash2)
	}

	hash3 := HashAPIKey("pfs_different-key")
	if hash1 == hash3 {
		t.Errorf("different inputs produced same hash")
	}

	if len(hash1) != 64 {
		t.Errorf("expected 64-char hex string, got %d chars", len(hash1))
	}
}

func TestJWTRoundTrip(t *testing.T) {
	secret := []byte("test-secret-key")
	tokenStr, err := GenerateJWT(secret, "user-1", "org-1", RoleEditor, "test@example.com", time.Hour)
	if err != nil {
		t.Fatalf("GenerateJWT: %v", err)
	}

	claims, err := ValidateJWT(secret, tokenStr)
	if err != nil {
		t.Fatalf("ValidateJWT: %v", err)
	}

	if claims.UserID != "user-1" {
		t.Errorf("UserID = %q, want %q", claims.UserID, "user-1")
	}
	if claims.OrgID != "org-1" {
		t.Errorf("OrgID = %q, want %q", claims.OrgID, "org-1")
	}
	if claims.Role != "editor" {
		t.Errorf("Role = %q, want %q", claims.Role, "editor")
	}
	if claims.Email != "test@example.com" {
		t.Errorf("Email = %q, want %q", claims.Email, "test@example.com")
	}
}

func TestJWTInvalidSecret(t *testing.T) {
	secret := []byte("correct-secret")
	wrong := []byte("wrong-secret")

	tokenStr, err := GenerateJWT(secret, "u", "o", RoleViewer, "e", time.Hour)
	if err != nil {
		t.Fatalf("GenerateJWT: %v", err)
	}

	_, err = ValidateJWT(wrong, tokenStr)
	if err == nil {
		t.Fatal("expected error with wrong secret")
	}
}

func TestJWTExpired(t *testing.T) {
	secret := []byte("test-secret")
	tokenStr, err := GenerateJWT(secret, "u", "o", RoleViewer, "e", -time.Hour)
	if err != nil {
		t.Fatalf("GenerateJWT: %v", err)
	}

	_, err = ValidateJWT(secret, tokenStr)
	if err == nil {
		t.Fatal("expected error for expired token")
	}
}

func TestHasMinRole(t *testing.T) {
	tests := []struct {
		user Role
		min  Role
		want bool
	}{
		{RoleOwner, RoleViewer, true},
		{RoleOwner, RoleOwner, true},
		{RoleAdmin, RoleEditor, true},
		{RoleEditor, RoleEditor, true},
		{RoleViewer, RoleEditor, false},
		{RoleViewer, RoleAdmin, false},
		{RoleEditor, RoleAdmin, false},
	}

	for _, tt := range tests {
		got := HasMinRole(tt.user, tt.min)
		if got != tt.want {
			t.Errorf("HasMinRole(%s, %s) = %v, want %v", tt.user, tt.min, got, tt.want)
		}
	}
}
