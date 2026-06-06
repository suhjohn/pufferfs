package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	appconfig "github.com/pufferfs/pufferfs/internal/config"
	"github.com/pufferfs/pufferfs/pkg/models"
	"github.com/spf13/cobra"
)

const (
	defaultManifestURL = "https://api.pufferfs.com/cli/version"
	updateCheckTTL     = 24 * time.Hour
)

type upgradeOptions struct {
	ManifestURL     string
	TargetVersion   string
	RestartServices bool
	Force           bool
}

func upgradeCmd() *cobra.Command {
	options := upgradeOptions{RestartServices: true}
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade a direct pufferfs CLI install",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := appconfig.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			return runUpgrade(cfg, options)
		},
	}
	cmd.Flags().StringVar(&options.ManifestURL, "manifest-url", "", "CLI release manifest URL")
	cmd.Flags().StringVar(&options.TargetVersion, "version", "", "Version to install; defaults to latest")
	cmd.Flags().BoolVar(&options.RestartServices, "restart-services", options.RestartServices, "Restart installed pufferfs services after upgrading")
	cmd.Flags().BoolVar(&options.Force, "force", false, "Install even when the current version is newer or equal")
	return cmd
}

func checkCLICompatibility(cmd *cobra.Command) error {
	if shouldSkipUpdateCheck(cmd) {
		return nil
	}
	cfg, err := appconfig.Load()
	if err != nil || strings.TrimSpace(cfg.Server.URL) == "" {
		return nil
	}

	manifest, err := fetchCLIReleaseManifest(manifestURL(cfg, ""))
	if err != nil {
		return nil
	}
	if manifest.Minimum != "" && compareVersions(version, manifest.Minimum) < 0 {
		return fmt.Errorf("pufferfs %s is no longer supported by this server; upgrade to %s or newer", displayVersion(version), displayVersion(manifest.Minimum))
	}
	if shouldPrintUpdateNotice(manifest) {
		fmt.Fprintf(os.Stderr, "pufferfs %s is available; run `pufferfs upgrade`.\n", displayVersion(manifest.Latest))
		_ = writeUpdateCheckCache()
	}
	return nil
}

func shouldSkipUpdateCheck(cmd *cobra.Command) bool {
	if cmd == nil {
		return true
	}
	if os.Getenv("PUFFERFS_NO_UPDATE_CHECK") != "" {
		return true
	}
	for c := cmd; c != nil; c = c.Parent() {
		switch c.Name() {
		case "completion", "help", "init", "upgrade":
			return true
		}
	}
	return false
}

func runUpgrade(cfg *appconfig.Config, options upgradeOptions) error {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return fmt.Errorf("self-upgrade is not supported on %s; install the new pufferfs binary manually", runtime.GOOS)
	}

	manifest, err := fetchCLIReleaseManifest(manifestURL(nil, options.ManifestURL))
	if err != nil {
		return err
	}
	target := normalizeVersion(options.TargetVersion)
	if target == "" {
		target = normalizeVersion(manifest.Latest)
	}
	if target == "" || target == "dev" {
		return fmt.Errorf("release manifest does not advertise an installable latest version")
	}
	if !options.Force && compareVersions(version, target) >= 0 {
		fmt.Printf("pufferfs %s is already installed.\n", displayVersion(version))
		return nil
	}

	download, err := downloadForTarget(manifest, target)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating current executable: %w", err)
	}

	fmt.Printf("Downloading pufferfs %s for %s/%s...\n", displayVersion(target), runtime.GOOS, runtime.GOARCH)
	tmpDir, err := os.MkdirTemp("", "pufferfs-upgrade-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	archivePath := filepath.Join(tmpDir, "pufferfs.tar.gz")
	if err := downloadFile(download.URL, archivePath); err != nil {
		return err
	}
	if download.SHA256 == "" {
		download.SHA256 = fetchChecksumForAsset(download.URL)
	}
	if download.SHA256 == "" {
		return fmt.Errorf("release manifest does not include a SHA-256 checksum for %s", platformKey())
	}
	if err := verifySHA256(archivePath, download.SHA256); err != nil {
		return err
	}

	newBinary := filepath.Join(tmpDir, "pufferfs")
	if err := extractPufferFSBinary(archivePath, newBinary); err != nil {
		return err
	}
	if err := replaceExecutable(exe, newBinary); err != nil {
		return err
	}
	fmt.Printf("Upgraded pufferfs to %s.\n", displayVersion(target))

	if options.RestartServices {
		if err := restartInstalledServices(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: restarting services: %v\n", err)
		}
	}
	_ = writeUpdateCheckCache()
	return nil
}

func manifestURL(cfg *appconfig.Config, override string) string {
	if strings.TrimSpace(override) != "" {
		return strings.TrimSpace(override)
	}
	if cfg != nil && strings.TrimSpace(cfg.Server.URL) != "" {
		return strings.TrimRight(strings.TrimSpace(cfg.Server.URL), "/") + "/cli/version"
	}
	return defaultManifestURL
}

func fetchCLIReleaseManifest(url string) (models.CLIReleaseManifest, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return models.CLIReleaseManifest{}, fmt.Errorf("fetching CLI release manifest: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return models.CLIReleaseManifest{}, fmt.Errorf("fetching CLI release manifest: HTTP %d: %s", resp.StatusCode, string(body))
	}
	var manifest models.CLIReleaseManifest
	if err := json.NewDecoder(resp.Body).Decode(&manifest); err != nil {
		return models.CLIReleaseManifest{}, fmt.Errorf("parsing CLI release manifest: %w", err)
	}
	manifest.Latest = normalizeVersion(manifest.Latest)
	manifest.Minimum = normalizeVersion(manifest.Minimum)
	return manifest, nil
}

func shouldPrintUpdateNotice(manifest models.CLIReleaseManifest) bool {
	if manifest.Latest == "" || compareVersions(version, manifest.Latest) >= 0 {
		return false
	}
	info, err := os.Stat(updateCheckCachePath())
	if err == nil && time.Since(info.ModTime()) < updateCheckTTL {
		return false
	}
	return true
}

func updateCheckCachePath() string {
	return filepath.Join(appconfig.DefaultConfigDir(), "update-check")
}

func writeUpdateCheckCache() error {
	path := updateCheckCachePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(time.Now().Format(time.RFC3339)+"\n"), 0o600)
}

func downloadForTarget(manifest models.CLIReleaseManifest, target string) (models.CLIDownload, error) {
	download, ok := manifest.Downloads[platformKey()]
	if !ok || strings.TrimSpace(download.URL) == "" {
		return models.CLIDownload{}, fmt.Errorf("release manifest has no download for %s", platformKey())
	}
	target = normalizeVersion(target)
	latest := normalizeVersion(manifest.Latest)
	if target != "" && latest != "" && target != latest {
		download.URL = retargetReleaseURL(download.URL, latest, target)
		download.SHA256 = ""
	}
	return download, nil
}

func retargetReleaseURL(rawURL, fromVersion, toVersion string) string {
	fromTag := "v" + strings.TrimPrefix(fromVersion, "v")
	toTag := "v" + strings.TrimPrefix(toVersion, "v")
	out := strings.Replace(rawURL, "/"+fromTag+"/", "/"+toTag+"/", 1)
	out = strings.ReplaceAll(out, "_"+strings.TrimPrefix(fromVersion, "v")+"_", "_"+strings.TrimPrefix(toVersion, "v")+"_")
	return out
}

func platformKey() string {
	return runtime.GOOS + "-" + runtime.GOARCH
}

func downloadFile(url, dest string) error {
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("downloading %s: HTTP %d: %s", url, resp.StatusCode, string(body))
	}
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, resp.Body); err != nil {
		return fmt.Errorf("writing download: %w", err)
	}
	return nil
}

func fetchChecksumForAsset(assetURL string) string {
	checksumsURL := checksumURL(assetURL)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(checksumsURL)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ""
	}
	assetName := filepath.Base(assetURL)
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == assetName {
			return fields[0]
		}
	}
	return ""
}

func checksumURL(assetURL string) string {
	idx := strings.LastIndex(assetURL, "/")
	if idx < 0 {
		return "checksums.txt"
	}
	return assetURL[:idx+1] + "checksums.txt"
}

func verifySHA256(path, want string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	got := hex.EncodeToString(hash.Sum(nil))
	want = strings.ToLower(strings.TrimSpace(want))
	if got != want {
		return fmt.Errorf("checksum mismatch for %s: got %s, want %s", filepath.Base(path), got, want)
	}
	return nil
}

func extractPufferFSBinary(archivePath, dest string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("opening release archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("reading release archive: %w", err)
		}
		if header.FileInfo().IsDir() || filepath.Base(header.Name) != "pufferfs" {
			continue
		}
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, tr); err != nil {
			_ = out.Close()
			return fmt.Errorf("extracting pufferfs binary: %w", err)
		}
		if err := out.Close(); err != nil {
			return err
		}
		return nil
	}
	return fmt.Errorf("release archive does not contain a pufferfs binary")
}

func replaceExecutable(current, next string) error {
	info, err := os.Stat(current)
	if err != nil {
		return err
	}
	if err := os.Chmod(next, info.Mode().Perm()|0o111); err != nil {
		return err
	}
	backup := current + ".old"
	_ = os.Remove(backup)
	if err := os.Rename(current, backup); err != nil {
		return fmt.Errorf("moving current binary aside: %w", err)
	}
	if err := os.Rename(next, current); err != nil {
		_ = os.Rename(backup, current)
		return fmt.Errorf("installing new binary: %w", err)
	}
	_ = os.Remove(backup)
	return nil
}

func restartInstalledServices() error {
	switch runtime.GOOS {
	case "darwin":
		return restartInstalledLaunchdServices()
	case "linux":
		return restartInstalledSystemdServices()
	default:
		return nil
	}
}

func restartInstalledLaunchdServices() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	matches, err := filepath.Glob(filepath.Join(home, "Library", "LaunchAgents", "ai.pufferfs.*.plist"))
	if err != nil {
		return err
	}
	var errs []string
	for _, path := range matches {
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "ai.pufferfs."), ".plist")
		if err := restartService(name); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func restartInstalledSystemdServices() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	matches, err := filepath.Glob(filepath.Join(home, ".config", "systemd", "user", "pufferfs-*.service"))
	if err != nil {
		return err
	}
	var errs []string
	for _, path := range matches {
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "pufferfs-"), ".service")
		if err := restartService(name); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func normalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	return v
}

func displayVersion(v string) string {
	v = normalizeVersion(v)
	if v == "" {
		return "dev"
	}
	if v == "dev" {
		return v
	}
	return "v" + v
}

func compareVersions(a, b string) int {
	a = normalizeVersion(a)
	b = normalizeVersion(b)
	if a == b {
		return 0
	}
	if a == "" || a == "dev" {
		return -1
	}
	if b == "" || b == "dev" {
		return 1
	}
	ap := parseVersion(a)
	bp := parseVersion(b)
	for i := 0; i < 3; i++ {
		if ap[i] < bp[i] {
			return -1
		}
		if ap[i] > bp[i] {
			return 1
		}
	}
	asuffix := versionSuffix(a)
	bsuffix := versionSuffix(b)
	if asuffix == bsuffix {
		return 0
	}
	if asuffix == "" {
		return 1
	}
	if bsuffix == "" {
		return -1
	}
	if asuffix < bsuffix {
		return -1
	}
	return 1
}

func parseVersion(v string) [3]int {
	base := v
	if idx := strings.IndexAny(base, "-+"); idx >= 0 {
		base = base[:idx]
	}
	parts := strings.Split(base, ".")
	var out [3]int
	for i := 0; i < len(parts) && i < 3; i++ {
		for _, r := range parts[i] {
			if r < '0' || r > '9' {
				break
			}
			out[i] = out[i]*10 + int(r-'0')
		}
	}
	return out
}

func versionSuffix(v string) string {
	if idx := strings.Index(v, "-"); idx >= 0 {
		return v[idx+1:]
	}
	return ""
}
