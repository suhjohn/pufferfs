package ignore

import (
	"os"
	"path/filepath"
	"testing"
)

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

func TestMatcherScopesNestedGitignore(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "with-gitignore", "media", "video"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "without-gitignore", "media", "video"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "with-gitignore", ".gitignore"), []byte("*.mp4\n*.mov\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	matcher := NewMatcher(root)
	for _, path := range []string{
		"with-gitignore/media/video/company_intro.mp4",
		"with-gitignore/media/video/product_teaser.mov",
	} {
		if !matcher.ShouldIgnore(path, false) {
			t.Fatalf("ShouldIgnore(%q) = false, want true", path)
		}
	}
	for _, path := range []string{
		"without-gitignore/media/video/company_intro.mp4",
		"without-gitignore/media/video/product_teaser.mov",
		"with-gitignore/media/audio/hold_message.mp3",
	} {
		if matcher.ShouldIgnore(path, false) {
			t.Fatalf("ShouldIgnore(%q) = true, want false", path)
		}
	}
}

func TestMatcherLoadsGlobalTpfsIgnore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".tpfs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".tpfs", ".tpfsignore"), []byte("global-cache/\n*.local\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	matcher := NewMatcher(t.TempDir())
	for _, path := range []string{
		"global-cache/index.bin",
		"settings.local",
	} {
		if !matcher.ShouldIgnore(path, false) {
			t.Fatalf("ShouldIgnore(%q) = false, want true", path)
		}
	}
}

func TestMatcherLoadsCentralPolicyPatterns(t *testing.T) {
	root := t.TempDir()
	matcher := NewMatcherWithPolicy(root, PolicyPatternSet{
		OrgPatterns:  "org-private/\n*.orgtmp\n",
		UserPatterns: "user-private/\n*.usertmp\n",
	})

	for _, path := range []string{
		"org-private/plan.md",
		"notes.orgtmp",
		"user-private/scratch.md",
		"notes.usertmp",
	} {
		if !matcher.ShouldIgnore(path, false) {
			t.Fatalf("ShouldIgnore(%q) = false, want true", path)
		}
	}
	if matcher.ShouldIgnore("shared/README.md", false) {
		t.Fatalf("central policy ignored shared file")
	}
}

func TestPolicyMatcherDoesNotLoadLocalRules(t *testing.T) {
	matcher := NewPolicyMatcher(PolicyPatternSet{
		OrgPatterns: "org-private/\n",
	})

	if !matcher.ShouldIgnore("org-private/a.txt", false) {
		t.Fatalf("policy matcher did not apply org rule")
	}
	if matcher.ShouldIgnore(".env", false) {
		t.Fatalf("policy matcher should not apply secret filename rules")
	}
	if matcher.ShouldIgnore("node_modules/pkg/index.js", false) {
		t.Fatalf("policy matcher should not apply default local rules")
	}
}
