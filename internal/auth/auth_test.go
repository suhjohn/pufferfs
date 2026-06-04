package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAdminMiddlewareAcceptsConfiguredKey(t *testing.T) {
	rawKey := "admin-secret"
	handler := AdminMiddleware(HashAPIKey(rawKey))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !IsAdmin(r.Context()) {
			t.Fatal("admin context was not set")
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodDelete, "/admin/orgs/org-1", nil)
	req.Header.Set("Authorization", "Bearer "+rawKey)
	resp := httptest.NewRecorder()

	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusNoContent)
	}
}

func TestAdminMiddlewareRejectsWrongKey(t *testing.T) {
	handler := AdminMiddleware(HashAPIKey("admin-secret"))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	}))

	req := httptest.NewRequest(http.MethodDelete, "/admin/orgs/org-1", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp := httptest.NewRecorder()

	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusUnauthorized)
	}
}
