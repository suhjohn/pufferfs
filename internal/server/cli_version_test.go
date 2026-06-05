package server

import (
	"testing"

	"github.com/pufferfs/pufferfs/pkg/models"
)

func TestCLIReleaseManifestFromEnv(t *testing.T) {
	t.Setenv("PUFFERFS_CLI_LATEST_VERSION", "v0.3.0")
	t.Setenv("PUFFERFS_CLI_MIN_VERSION", "0.2.0")
	t.Setenv("PUFFERFS_CLI_DOWNLOAD_BASE_URL", "https://downloads.example.com/releases")
	t.Setenv("PUFFERFS_CLI_SHA256_DARWIN_ARM64", "abc123")

	manifest := cliReleaseManifestFromEnv()
	if manifest.Latest != "0.3.0" {
		t.Fatalf("latest = %q, want 0.3.0", manifest.Latest)
	}
	if manifest.Minimum != "0.2.0" {
		t.Fatalf("minimum = %q, want 0.2.0", manifest.Minimum)
	}
	if manifest.ProtocolMin != models.SyncProtocolVersion || manifest.ProtocolMax != models.SyncProtocolVersion {
		t.Fatalf("protocol range = %d..%d, want %d", manifest.ProtocolMin, manifest.ProtocolMax, models.SyncProtocolVersion)
	}
	download := manifest.Downloads["darwin-arm64"]
	if download.URL != "https://downloads.example.com/releases/v0.3.0/pufferfs_0.3.0_darwin_arm64.tar.gz" {
		t.Fatalf("darwin-arm64 URL = %q", download.URL)
	}
	if download.SHA256 != "abc123" {
		t.Fatalf("darwin-arm64 SHA256 = %q, want abc123", download.SHA256)
	}
}
