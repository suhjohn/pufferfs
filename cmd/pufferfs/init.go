package main

import (
	"context"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	appconfig "github.com/pufferfs/pufferfs/internal/config"
	"github.com/spf13/cobra"
)

const defaultServerURL = "https://api.pufferfs.com"

type initOptions struct {
	ServerURL string
	APIKey    string
	Manual    bool
	NoBrowser bool
}

type cliAuthResult struct {
	APIKey string
	Email  string
}

func initCmd() *cobra.Command {
	options := initOptions{
		ServerURL: initDefaultServerURL(),
	}
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Connect the PufferFS CLI to your account",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInit(options)
		},
	}
	cmd.Flags().StringVar(&options.ServerURL, "server-url", options.ServerURL, "PufferFS API server URL")
	cmd.Flags().StringVar(&options.APIKey, "api-key", "", "API key to write without opening the browser login flow")
	cmd.Flags().BoolVar(&options.Manual, "manual", false, "Write config without logging in")
	cmd.Flags().BoolVar(&options.NoBrowser, "no-browser", false, "Print the login URL instead of opening a browser")
	return cmd
}

func runInit(options initOptions) error {
	serverURL, err := normalizeServerURL(options.ServerURL)
	if err != nil {
		return err
	}
	cfg := defaultInitConfig(serverURL)

	if strings.TrimSpace(options.APIKey) != "" {
		cfg.Server.APIKey = strings.TrimSpace(options.APIKey)
		if err := appconfig.Save(cfg); err != nil {
			return fmt.Errorf("saving config: %w", err)
		}
		fmt.Printf("Config written to %s\n", appconfig.ConfigPath())
		fmt.Println("PufferFS CLI connected.")
		return nil
	}

	if options.Manual {
		if err := appconfig.Save(cfg); err != nil {
			return fmt.Errorf("saving config: %w", err)
		}
		fmt.Printf("Config written to %s\n", appconfig.ConfigPath())
		fmt.Println("Add an API key with `pufferfs init --api-key pfs_...` or edit the config file.")
		return nil
	}

	result, err := runBrowserLogin(serverURL, !options.NoBrowser)
	if err != nil {
		return err
	}
	cfg.Server.APIKey = result.APIKey
	if err := appconfig.Save(cfg); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}

	fmt.Printf("Config written to %s\n", appconfig.ConfigPath())
	if result.Email != "" {
		fmt.Printf("PufferFS CLI connected as %s.\n", result.Email)
	} else {
		fmt.Println("PufferFS CLI connected.")
	}
	return nil
}

func defaultInitConfig(serverURL string) *appconfig.Config {
	return &appconfig.Config{
		Server: appconfig.ServerConfig{
			URL: serverURL,
		},
		Turbopuffer: appconfig.TurbopufferConfig{
			Region: "gcp-us-central1",
		},
		Storage: appconfig.StorageConfig{
			Bucket: "pufferfs",
		},
	}
}

func initDefaultServerURL() string {
	if v := strings.TrimSpace(os.Getenv("PUFFERFS_SERVER_URL")); v != "" {
		return v
	}
	return defaultServerURL
}

func normalizeServerURL(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "", fmt.Errorf("server URL is required")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid server URL: %q", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("server URL must use http or https")
	}
	return raw, nil
}

func runBrowserLogin(serverURL string, openBrowser bool) (cliAuthResult, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return cliAuthResult{}, fmt.Errorf("starting local callback server: %w", err)
	}

	resultCh := make(chan cliAuthResult, 1)
	errorCh := make(chan error, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if message := strings.TrimSpace(query.Get("error")); message != "" {
			errorCh <- fmt.Errorf("login failed: %s", message)
			writeCLIAuthPage(w, "PufferFS CLI login failed", "Return to your terminal and run pufferfs init again.")
			return
		}
		apiKey := strings.TrimSpace(query.Get("api_key"))
		if apiKey == "" {
			errorCh <- fmt.Errorf("login callback did not include an API key")
			writeCLIAuthPage(w, "PufferFS CLI login failed", "The callback did not include a CLI key. Return to your terminal and run pufferfs init again.")
			return
		}
		resultCh <- cliAuthResult{
			APIKey: apiKey,
			Email:  strings.TrimSpace(query.Get("email")),
		}
		writeCLIAuthPage(w, "PufferFS CLI connected", "Your scoped CLI key has been issued and saved locally. You can close this window and return to the terminal.")
	})

	server := &http.Server{Handler: mux}
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			errorCh <- fmt.Errorf("local callback server: %w", err)
		}
	}()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()

	port := listener.Addr().(*net.TCPAddr).Port
	callbackURL := fmt.Sprintf("http://127.0.0.1:%d/callback", port)
	loginURL := serverURL + "/auth/google?cli_redirect_uri=" + url.QueryEscape(callbackURL)

	if openBrowser {
		fmt.Println("Opening browser to connect your PufferFS account...")
		if err := openURL(loginURL); err != nil {
			fmt.Printf("Open this URL to continue:\n%s\n", loginURL)
		}
	} else {
		fmt.Printf("Open this URL to connect your PufferFS account:\n%s\n", loginURL)
	}

	select {
	case result := <-resultCh:
		return result, nil
	case err := <-errorCh:
		return cliAuthResult{}, err
	case <-time.After(5 * time.Minute):
		return cliAuthResult{}, fmt.Errorf("login timed out")
	}
}

func openURL(target string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	return cmd.Start()
}

func writeCLIAuthPage(w http.ResponseWriter, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s</title>
<style>
:root { color-scheme: dark; }
* { box-sizing: border-box; }
body {
  min-height: 100vh;
  margin: 0;
  display: grid;
  place-items: center;
  padding: 2rem;
  color: #f2f2f2;
  background: #101010;
  font: 15px/1.5 ui-monospace, "SF Mono", Menlo, Consolas, monospace;
}
main {
  width: min(100%%, 34rem);
  padding: 2rem;
  border: 3px double rgba(242, 242, 242, 0.8);
  background: #171717;
}
.logo {
  width: 42px;
  height: 42px;
  margin-bottom: 1rem;
}
h1 {
  margin: 0 0 0.75rem;
  font-size: clamp(1.25rem, 4vw, 1.7rem);
  line-height: 1.15;
}
p {
  margin: 0;
  color: #ababab;
}
</style>
</head>
<body>
<main>
<svg class="logo" viewBox="0 0 64 64" aria-hidden="true">
<path fill="#f2f2f2" d="M8 20h18v-4h16v4h14v6H8z"/>
<path fill="#f2f2f2" d="M8 26h48v30H8z"/>
<path fill="#101010" d="M12 30h40v20H12z"/>
<path fill="#f2f2f2" d="M14 32h36v16H14z"/>
</svg>
<h1>%s</h1>
<p>%s</p>
</main>
</body>
</html>`, html.EscapeString(title), html.EscapeString(title), html.EscapeString(body))
}
