package ignore

import "testing"

func TestMatcherIgnoresSecretFilesByDefault(t *testing.T) {
	matcher := NewMatcher(t.TempDir())

	for _, path := range []string{
		".env",
		".env.local",
		"config/prod.pem",
		"secrets/id_rsa",
		"service-account-prod.json",
		"packages/app/.npmrc",
	} {
		if !matcher.ShouldIgnore(path, false) {
			t.Fatalf("ShouldIgnore(%q) = false, want true", path)
		}
	}
}

func TestMatcherDoesNotIgnoreNormalFilesAsSecrets(t *testing.T) {
	matcher := NewMatcher(t.TempDir())

	for _, path := range []string{
		"README.md",
		"docs/environment.md",
		"src/config.go",
	} {
		if matcher.ShouldIgnore(path, false) {
			t.Fatalf("ShouldIgnore(%q) = true, want false", path)
		}
	}
}
