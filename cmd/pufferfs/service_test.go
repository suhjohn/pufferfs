package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizeServiceName(t *testing.T) {
	tests := map[string]string{
		"My Repo":          "my-repo",
		"repo_123":         "repo-123",
		"---":              "default",
		" Team / Project ": "team-project",
	}
	for input, want := range tests {
		if got := sanitizeServiceName(input); got != want {
			t.Fatalf("sanitizeServiceName(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestServiceCommandIncludesLifecycleCommands(t *testing.T) {
	cmd := serviceCmd()
	want := map[string]bool{
		"install":   true,
		"start":     true,
		"restart":   true,
		"stop":      true,
		"status":    true,
		"logs":      true,
		"uninstall": true,
	}
	for _, child := range cmd.Commands() {
		use := strings.Fields(child.Use)[0]
		delete(want, use)
	}
	if len(want) != 0 {
		t.Fatalf("service command missing subcommands: %#v", want)
	}
}

func TestServiceActionCmdSanitizesName(t *testing.T) {
	var got string
	cmd := serviceActionCmd("test", "test service action", func(name string) error {
		got = name
		return nil
	})
	cmd.SetArgs([]string{" Team / Project "})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute action: %v", err)
	}
	if got != "team-project" {
		t.Fatalf("sanitized service name = %q, want team-project", got)
	}
}

func TestServiceActionCmdReturnsActionError(t *testing.T) {
	wantErr := errors.New("boom")
	cmd := serviceActionCmd("test", "test service action", func(name string) error {
		return wantErr
	})
	cmd.SetArgs([]string{"repo"})
	if err := cmd.Execute(); !errors.Is(err, wantErr) {
		t.Fatalf("execute error = %v, want %v", err, wantErr)
	}
}

func TestBuildServiceSpecDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	spec, err := buildServiceSpec(filepath.Join(home, "My Repo"), serviceInstallOptions{
		RootName:             "My Repo",
		Debounce:             "2s",
		MaxBackoff:           "1m",
		MaxSameFailures:      8,
		MaxSameFailureWindow: "10m",
	})
	if err != nil {
		t.Fatalf("buildServiceSpec: %v", err)
	}
	if spec.Name != "my-repo" || spec.Label != "ai.pufferfs.my-repo" {
		t.Fatalf("spec name/label = %q/%q", spec.Name, spec.Label)
	}
	if !strings.HasSuffix(spec.LogPath, filepath.Join(".pufferfs", "logs", "my-repo.log")) {
		t.Fatalf("log path = %q", spec.LogPath)
	}
}

func TestRenderSystemdUserService(t *testing.T) {
	spec := serviceSpec{
		Name:                 "repo",
		Path:                 "/tmp/my repo",
		Executable:           "/usr/local/bin/pufferfs",
		RootName:             "repo",
		NoVector:             true,
		Debounce:             "2s",
		MaxBackoff:           "1m",
		MaxSameFailures:      8,
		MaxSameFailureWindow: "10m",
		Home:                 "/home/test user",
		LogPath:              "/home/test user/.pufferfs/logs/repo.log",
		ErrorLogPath:         "/home/test user/.pufferfs/logs/repo.err.log",
	}
	unit := renderSystemdUserService(spec)
	for _, want := range []string{
		"ExecStart=/usr/local/bin/pufferfs sync",
		"--follow",
		"--no-vector",
		"--max-same-failures 8",
		`WorkingDirectory="/tmp/my repo"`,
		`Environment=HOME="/home/test user"`,
		"Restart=on-failure",
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("systemd unit missing %q:\n%s", want, unit)
		}
	}
}

func TestRenderLaunchdPlist(t *testing.T) {
	spec := serviceSpec{
		Name:                 "repo",
		Label:                "ai.pufferfs.repo",
		Path:                 "/tmp/repo",
		Executable:           "/usr/local/bin/pufferfs",
		RootName:             "repo",
		Debounce:             "2s",
		MaxBackoff:           "1m",
		MaxSameFailures:      8,
		MaxSameFailureWindow: "10m",
		LogPath:              "/tmp/repo.log",
		ErrorLogPath:         "/tmp/repo.err.log",
	}
	data, err := renderLaunchdPlist(spec)
	if err != nil {
		t.Fatalf("renderLaunchdPlist: %v", err)
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		if _, err := dec.Token(); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("plist is not valid XML: %v\n%s", err, string(data))
		}
	}
	for _, want := range []string{
		"<key>Label</key>",
		"<string>ai.pufferfs.repo</string>",
		"<string>--follow</string>",
		"<key>KeepAlive</key>",
	} {
		if !bytes.Contains(data, []byte(want)) {
			t.Fatalf("plist missing %q:\n%s", want, string(data))
		}
	}
}

func TestBuildServiceSpecRejectsBadDurations(t *testing.T) {
	_, err := buildServiceSpec(os.TempDir(), serviceInstallOptions{
		RootName:             "repo",
		Debounce:             "nope",
		MaxBackoff:           "1m",
		MaxSameFailures:      8,
		MaxSameFailureWindow: "10m",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid --debounce") {
		t.Fatalf("err = %v, want invalid debounce", err)
	}
}
