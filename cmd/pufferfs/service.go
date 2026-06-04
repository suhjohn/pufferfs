package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"
)

func serviceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Install and manage a long-running sync service",
	}
	cmd.AddCommand(serviceInstallCmd())
	cmd.AddCommand(serviceStartCmd())
	cmd.AddCommand(serviceRestartCmd())
	cmd.AddCommand(serviceStopCmd())
	cmd.AddCommand(serviceStatusCmd())
	cmd.AddCommand(serviceLogsCmd())
	cmd.AddCommand(serviceUninstallCmd())
	return cmd
}

type serviceInstallOptions struct {
	RootName             string
	RootID               string
	ServiceName          string
	Debounce             string
	MaxBackoff           string
	MaxSameFailures      int
	MaxSameFailureWindow string
}

type serviceSpec struct {
	Name                 string
	Label                string
	Path                 string
	Executable           string
	RootName             string
	RootID               string
	Debounce             string
	MaxBackoff           string
	MaxSameFailures      int
	MaxSameFailureWindow string
	Home                 string
	LogPath              string
	ErrorLogPath         string
}

func serviceInstallCmd() *cobra.Command {
	var options serviceInstallOptions
	options.Debounce = defaultFollowOptions().Debounce.String()
	options.MaxBackoff = defaultFollowOptions().MaxBackoff.String()
	options.MaxSameFailures = defaultFollowOptions().MaxSameFailures
	options.MaxSameFailureWindow = defaultFollowOptions().MaxSameFailureWindow.String()

	cmd := &cobra.Command{
		Use:   "install [path]",
		Short: "Install a user service that runs pufferfs sync --follow",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) > 0 {
				dir = args[0]
			}
			absDir, err := filepath.Abs(dir)
			if err != nil {
				return err
			}
			spec, err := buildServiceSpec(absDir, options)
			if err != nil {
				return err
			}
			path, err := installService(spec)
			if err != nil {
				return err
			}
			fmt.Printf("Installed %s service %q at %s\n", servicePlatformName(), spec.Name, path)
			fmt.Printf("Start it with: pufferfs service start %s\n", spec.Name)
			return nil
		},
	}
	cmd.Flags().StringVarP(&options.RootName, "name", "n", "", "Name alias for this root")
	cmd.Flags().StringVar(&options.RootID, "id", "", "Root ID to re-attach to")
	cmd.Flags().StringVar(&options.ServiceName, "service-name", "", "Service name; defaults to the root name")
	cmd.Flags().StringVar(&options.Debounce, "debounce", options.Debounce, "Debounce interval for file changes")
	cmd.Flags().StringVar(&options.MaxBackoff, "max-backoff", options.MaxBackoff, "Maximum retry backoff")
	cmd.Flags().IntVar(&options.MaxSameFailures, "max-same-failures", options.MaxSameFailures, "Exit after this many consecutive identical sync failures")
	cmd.Flags().StringVar(&options.MaxSameFailureWindow, "max-same-failure-window", options.MaxSameFailureWindow, "Exit after identical sync failures persist for this long")
	return cmd
}

func serviceStartCmd() *cobra.Command {
	return serviceActionCmd("start", "Start an installed sync service", startService)
}

func serviceRestartCmd() *cobra.Command {
	return serviceActionCmd("restart", "Restart an installed sync service", restartService)
}

func serviceStopCmd() *cobra.Command {
	return serviceActionCmd("stop", "Stop an installed sync service", stopService)
}

func serviceStatusCmd() *cobra.Command {
	return serviceActionCmd("status", "Show sync service status", statusService)
}

func serviceLogsCmd() *cobra.Command {
	return serviceActionCmd("logs", "Follow sync service logs", logsService)
}

func serviceUninstallCmd() *cobra.Command {
	return serviceActionCmd("uninstall", "Uninstall a sync service", uninstallService)
}

func serviceActionCmd(use, short string, action func(string) error) *cobra.Command {
	return &cobra.Command{
		Use:   use + " <service-name>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return action(sanitizeServiceName(args[0]))
		},
	}
}

func buildServiceSpec(absDir string, options serviceInstallOptions) (serviceSpec, error) {
	if _, err := time.ParseDuration(options.Debounce); err != nil {
		return serviceSpec{}, fmt.Errorf("invalid --debounce: %w", err)
	}
	if _, err := time.ParseDuration(options.MaxBackoff); err != nil {
		return serviceSpec{}, fmt.Errorf("invalid --max-backoff: %w", err)
	}
	if _, err := time.ParseDuration(options.MaxSameFailureWindow); err != nil {
		return serviceSpec{}, fmt.Errorf("invalid --max-same-failure-window: %w", err)
	}
	if options.MaxSameFailures <= 0 {
		return serviceSpec{}, fmt.Errorf("--max-same-failures must be greater than zero")
	}
	exe, err := os.Executable()
	if err != nil {
		return serviceSpec{}, fmt.Errorf("locating pufferfs executable: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return serviceSpec{}, err
	}
	rootName := options.RootName
	if rootName == "" {
		rootName = filepath.Base(absDir)
	}
	name := options.ServiceName
	if name == "" {
		name = rootName
	}
	name = sanitizeServiceName(name)
	label := "ai.pufferfs." + name
	logDir := filepath.Join(home, ".pufferfs", "logs")
	return serviceSpec{
		Name:                 name,
		Label:                label,
		Path:                 absDir,
		Executable:           exe,
		RootName:             rootName,
		RootID:               options.RootID,
		Debounce:             options.Debounce,
		MaxBackoff:           options.MaxBackoff,
		MaxSameFailures:      options.MaxSameFailures,
		MaxSameFailureWindow: options.MaxSameFailureWindow,
		Home:                 home,
		LogPath:              filepath.Join(logDir, name+".log"),
		ErrorLogPath:         filepath.Join(logDir, name+".err.log"),
	}, nil
}

func sanitizeServiceName(name string) string {
	name = strings.TrimSpace(strings.ToLower(name))
	var b strings.Builder
	lastDash := false
	for _, r := range name {
		ok := unicode.IsLetter(r) || unicode.IsDigit(r)
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "default"
	}
	return out
}

func installService(spec serviceSpec) (string, error) {
	switch runtime.GOOS {
	case "linux":
		return installSystemdUserService(spec)
	case "darwin":
		return installLaunchdService(spec)
	default:
		return "", fmt.Errorf("pufferfs service install is not supported on %s; run `pufferfs sync %s --name %s --follow` under your process supervisor", runtime.GOOS, spec.Path, spec.RootName)
	}
}

func startService(name string) error {
	switch runtime.GOOS {
	case "linux":
		return runServiceCommand("systemctl", "--user", "start", systemdUnitName(name))
	case "darwin":
		return runServiceCommand("launchctl", "bootstrap", launchdDomain(), launchdPlistPath(name))
	default:
		return fmt.Errorf("service start is not supported on %s", runtime.GOOS)
	}
}

func stopService(name string) error {
	switch runtime.GOOS {
	case "linux":
		return runServiceCommand("systemctl", "--user", "stop", systemdUnitName(name))
	case "darwin":
		return runServiceCommand("launchctl", "bootout", launchdDomain(), launchdPlistPath(name))
	default:
		return fmt.Errorf("service stop is not supported on %s", runtime.GOOS)
	}
}

func restartService(name string) error {
	switch runtime.GOOS {
	case "linux":
		return runServiceCommand("systemctl", "--user", "restart", systemdUnitName(name))
	case "darwin":
		return runServiceCommand("launchctl", "kickstart", "-k", launchdDomain()+"/ai.pufferfs."+name)
	default:
		return fmt.Errorf("service restart is not supported on %s", runtime.GOOS)
	}
}

func statusService(name string) error {
	switch runtime.GOOS {
	case "linux":
		return runServiceCommand("systemctl", "--user", "status", "--no-pager", systemdUnitName(name))
	case "darwin":
		return runServiceCommand("launchctl", "print", launchdDomain()+"/ai.pufferfs."+name)
	default:
		return fmt.Errorf("service status is not supported on %s", runtime.GOOS)
	}
}

func logsService(name string) error {
	switch runtime.GOOS {
	case "linux":
		return runServiceCommand("journalctl", "--user", "-u", systemdUnitName(name), "-n", "100", "-f")
	case "darwin":
		return runServiceCommand("tail", "-n", "100", "-f", launchdLogPath(name), launchdErrorLogPath(name))
	default:
		return fmt.Errorf("service logs is not supported on %s", runtime.GOOS)
	}
}

func uninstallService(name string) error {
	switch runtime.GOOS {
	case "linux":
		return uninstallSystemdUserService(name)
	case "darwin":
		return uninstallLaunchdService(name)
	default:
		return fmt.Errorf("service uninstall is not supported on %s", runtime.GOOS)
	}
}

func installSystemdUserService(spec serviceSpec) (string, error) {
	path := systemdUserServicePath(spec.Name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(spec.LogPath), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(renderSystemdUserService(spec)), 0o600); err != nil {
		return "", err
	}
	if err := runServiceCommand("systemctl", "--user", "daemon-reload"); err != nil {
		return "", err
	}
	if err := runServiceCommand("systemctl", "--user", "enable", systemdUnitName(spec.Name)); err != nil {
		return "", err
	}
	return path, nil
}

func uninstallSystemdUserService(name string) error {
	path := systemdUserServicePath(name)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("service %q is not installed at %s", name, path)
		}
		return err
	}

	unit := systemdUnitName(name)
	_ = runServiceCommandSilent("systemctl", "--user", "stop", unit)
	if err := runServiceCommand("systemctl", "--user", "disable", unit); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := runServiceCommand("systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	_ = runServiceCommandSilent("systemctl", "--user", "reset-failed", unit)
	fmt.Printf("Uninstalled systemd user service %q from %s\n", name, path)
	return nil
}

func renderSystemdUserService(spec serviceSpec) string {
	args := serviceProgramArgs(spec)
	var quoted []string
	for _, arg := range args {
		quoted = append(quoted, systemdQuote(arg))
	}
	return fmt.Sprintf(`[Unit]
Description=PufferFS follow sync for %s
After=network-online.target

[Service]
Type=simple
WorkingDirectory=%s
Environment=HOME=%s
ExecStart=%s
Restart=on-failure
RestartSec=10s
StandardOutput=append:%s
StandardError=append:%s

[Install]
WantedBy=default.target
`, spec.Name, systemdQuote(spec.Path), systemdQuote(spec.Home), strings.Join(quoted, " "), systemdQuote(spec.LogPath), systemdQuote(spec.ErrorLogPath))
}

func installLaunchdService(spec serviceSpec) (string, error) {
	path := launchdPlistPath(spec.Name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(spec.LogPath), 0o700); err != nil {
		return "", err
	}
	data, err := renderLaunchdPlist(spec)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func uninstallLaunchdService(name string) error {
	path := launchdPlistPath(name)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("service %q is not installed at %s", name, path)
		}
		return err
	}

	_ = runServiceCommandSilent("launchctl", "bootout", launchdDomain(), path)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Printf("Uninstalled launchd user service %q from %s\n", name, path)
	return nil
}

func renderLaunchdPlist(spec serviceSpec) ([]byte, error) {
	doc := launchdPlist{
		Version: "1.0",
		Dict: launchdDict{
			Label:               spec.Label,
			ProgramArguments:    serviceProgramArgs(spec),
			WorkingDirectory:    spec.Path,
			StandardOutPath:     spec.LogPath,
			StandardErrorPath:   spec.ErrorLogPath,
			RunAtLoad:           true,
			KeepAliveSuccessful: false,
		},
	}
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	buf.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "https://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	enc := xml.NewEncoder(&buf)
	enc.Indent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

type launchdPlist struct {
	XMLName xml.Name    `xml:"plist"`
	Version string      `xml:"version,attr"`
	Dict    launchdDict `xml:"dict"`
}

type launchdDict struct {
	Label               string
	ProgramArguments    []string
	WorkingDirectory    string
	StandardOutPath     string
	StandardErrorPath   string
	RunAtLoad           bool
	KeepAliveSuccessful bool
}

func (d launchdDict) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	if err := e.EncodeToken(start); err != nil {
		return err
	}
	writeStringKey := func(key, value string) error {
		if err := e.EncodeElement(key, xml.StartElement{Name: xml.Name{Local: "key"}}); err != nil {
			return err
		}
		return e.EncodeElement(value, xml.StartElement{Name: xml.Name{Local: "string"}})
	}
	writeBoolKey := func(key string, value bool) error {
		if err := e.EncodeElement(key, xml.StartElement{Name: xml.Name{Local: "key"}}); err != nil {
			return err
		}
		name := "false"
		if value {
			name = "true"
		}
		return e.EncodeElement("", xml.StartElement{Name: xml.Name{Local: name}})
	}
	if err := writeStringKey("Label", d.Label); err != nil {
		return err
	}
	if err := e.EncodeElement("ProgramArguments", xml.StartElement{Name: xml.Name{Local: "key"}}); err != nil {
		return err
	}
	if err := e.EncodeToken(xml.StartElement{Name: xml.Name{Local: "array"}}); err != nil {
		return err
	}
	for _, arg := range d.ProgramArguments {
		if err := e.EncodeElement(arg, xml.StartElement{Name: xml.Name{Local: "string"}}); err != nil {
			return err
		}
	}
	if err := e.EncodeToken(xml.EndElement{Name: xml.Name{Local: "array"}}); err != nil {
		return err
	}
	for _, item := range []struct {
		key   string
		value string
	}{
		{"WorkingDirectory", d.WorkingDirectory},
		{"StandardOutPath", d.StandardOutPath},
		{"StandardErrorPath", d.StandardErrorPath},
	} {
		if err := writeStringKey(item.key, item.value); err != nil {
			return err
		}
	}
	if err := writeBoolKey("RunAtLoad", d.RunAtLoad); err != nil {
		return err
	}
	if err := e.EncodeElement("KeepAlive", xml.StartElement{Name: xml.Name{Local: "key"}}); err != nil {
		return err
	}
	if err := e.EncodeToken(xml.StartElement{Name: xml.Name{Local: "dict"}}); err != nil {
		return err
	}
	if err := writeBoolKey("SuccessfulExit", d.KeepAliveSuccessful); err != nil {
		return err
	}
	if err := e.EncodeToken(xml.EndElement{Name: xml.Name{Local: "dict"}}); err != nil {
		return err
	}
	return e.EncodeToken(start.End())
}

func serviceProgramArgs(spec serviceSpec) []string {
	args := []string{spec.Executable, "sync", spec.Path, "--follow", "--name", spec.RootName}
	if spec.RootID != "" {
		args = append(args, "--id", spec.RootID)
	}
	args = append(args,
		"--debounce", spec.Debounce,
		"--max-backoff", spec.MaxBackoff,
		"--max-same-failures", strconv.Itoa(spec.MaxSameFailures),
		"--max-same-failure-window", spec.MaxSameFailureWindow,
	)
	return args
}

func systemdQuote(arg string) string {
	if arg == "" {
		return `""`
	}
	if strings.IndexFunc(arg, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(`"'$\`, r)
	}) < 0 {
		return arg
	}
	return strconv.Quote(arg)
}

func runServiceCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func runServiceCommandSilent(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	return cmd.Run()
}

func servicePlatformName() string {
	switch runtime.GOOS {
	case "linux":
		return "systemd user"
	case "darwin":
		return "launchd user"
	default:
		return runtime.GOOS
	}
}

func systemdUnitName(name string) string {
	return "pufferfs-" + sanitizeServiceName(name) + ".service"
}

func systemdUserServicePath(name string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user", systemdUnitName(name))
}

func launchdDomain() string {
	return fmt.Sprintf("gui/%d", os.Getuid())
}

func launchdPlistPath(name string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", "ai.pufferfs."+sanitizeServiceName(name)+".plist")
}

func launchdLogPath(name string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".pufferfs", "logs", sanitizeServiceName(name)+".log")
}

func launchdErrorLogPath(name string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".pufferfs", "logs", sanitizeServiceName(name)+".err.log")
}
