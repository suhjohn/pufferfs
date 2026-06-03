// Package config manages PufferFs configuration.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Config is the top-level configuration.
type Config struct {
	Server      ServerConfig      `toml:"server"`
	Turbopuffer TurbopufferConfig `toml:"turbopuffer"`
	Storage     StorageConfig     `toml:"storage"`
}

type ServerConfig struct {
	URL    string `toml:"url"`
	APIKey string `toml:"api_key"`
}

type TurbopufferConfig struct {
	APIKey string `toml:"api_key"`
	Region string `toml:"region"`
}

type StorageConfig struct {
	EndpointURL    string `toml:"endpoint_url"`
	Bucket         string `toml:"bucket"`
	AccessKeyID    string `toml:"access_key_id"`
	SecretAccessKey string `toml:"secret_access_key"`
}

// DefaultConfigDir returns ~/.tpfs.
func DefaultConfigDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".tpfs"
	}
	return filepath.Join(home, ".tpfs")
}

// ConfigPath returns the default config file path.
func ConfigPath() string {
	return filepath.Join(DefaultConfigDir(), "config.toml")
}

// RootDir returns ~/.tpfs/roots/<rootID>.
func RootDir(rootID string) string {
	return filepath.Join(DefaultConfigDir(), "roots", rootID)
}

// Load reads the config from disk.
func Load() (*Config, error) {
	path := ConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	// Override from environment variables
	cfg.applyEnvOverrides()
	return &cfg, nil
}

// Save writes the config to disk.
func Save(cfg *Config) error {
	dir := DefaultConfigDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	f, err := os.Create(ConfigPath())
	if err != nil {
		return err
	}
	defer f.Close()

	enc := toml.NewEncoder(f)
	return enc.Encode(cfg)
}

func (c *Config) applyEnvOverrides() {
	if v := os.Getenv("PUFFERFS_SERVER_URL"); v != "" {
		c.Server.URL = v
	}
	if v := os.Getenv("PUFFERFS_API_KEY"); v != "" {
		c.Server.APIKey = v
	}
	if v := os.Getenv("TURBOPUFFER_API_KEY"); v != "" {
		c.Turbopuffer.APIKey = v
	}
	if v := os.Getenv("AWS_ENDPOINT_URL"); v != "" {
		c.Storage.EndpointURL = v
	}
	if v := os.Getenv("AWS_BUCKET_NAME"); v != "" {
		c.Storage.Bucket = v
	}
	if v := os.Getenv("AWS_ACCESS_KEY_ID"); v != "" {
		c.Storage.AccessKeyID = v
	}
	if v := os.Getenv("AWS_SECRET_ACCESS_KEY"); v != "" {
		c.Storage.SecretAccessKey = v
	}
}
