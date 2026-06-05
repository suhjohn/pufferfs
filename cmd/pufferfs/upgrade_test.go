package main

import "testing"

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a    string
		b    string
		want int
	}{
		{"dev", "0.1.0", -1},
		{"0.2.0", "0.1.9", 1},
		{"v0.2.0", "0.2.0", 0},
		{"0.2.0-alpha", "0.2.0", -1},
		{"0.2.1", "0.2.1", 0},
	}
	for _, tt := range tests {
		got := compareVersions(tt.a, tt.b)
		if got < 0 {
			got = -1
		} else if got > 0 {
			got = 1
		}
		if got != tt.want {
			t.Fatalf("compareVersions(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestRetargetReleaseURL(t *testing.T) {
	got := retargetReleaseURL(
		"https://github.com/suhjohn/pufferfs/releases/download/v0.3.0/pufferfs_0.3.0_darwin_arm64.tar.gz",
		"0.3.0",
		"0.4.0",
	)
	want := "https://github.com/suhjohn/pufferfs/releases/download/v0.4.0/pufferfs_0.4.0_darwin_arm64.tar.gz"
	if got != want {
		t.Fatalf("retargetReleaseURL = %q, want %q", got, want)
	}
}
