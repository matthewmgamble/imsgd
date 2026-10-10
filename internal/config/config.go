// Package config loads imsgd configuration from a YAML file with environment
// variable overrides.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// APIKey is a named credential. Each consuming application should get its own
// key so it can be revoked independently.
type APIKey struct {
	Name string `yaml:"name"`
	Key  string `yaml:"key"`
}

type TLS struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

type Contacts struct {
	Enabled bool `yaml:"enabled"`
	// Region is the default phone region (ISO 3166 code, e.g. "CA") for
	// numbers saved without a country code. Default: the Mac's region.
	Region string `yaml:"region"`
}

type Config struct {
	// Listen is the host part of the listen address; empty means all interfaces.
	Listen   string   `yaml:"listen"`
	Port     int      `yaml:"port"`
	DataDir  string   `yaml:"data_dir"`
	LogLevel string   `yaml:"log_level"`
	LogFile  string   `yaml:"log_file"`
	APIKeys  []APIKey `yaml:"api_keys"`
	TLS      TLS      `yaml:"tls"`
	Contacts Contacts `yaml:"contacts"`

	// MessagesDB overrides the chat.db path (useful for tests and fixtures).
	MessagesDB string `yaml:"messages_db"`

	// UploadMaxMB limits the size of a send request (attachments included).
	UploadMaxMB int64 `yaml:"upload_max_mb"`

	// CacheMaxMB limits the converted-attachment cache.
	CacheMaxMB int64 `yaml:"cache_max_mb"`

	// CORSOrigins lets browser apps on these origins call the API
	// (e.g. "http://192.168.1.50:3000"; "*" allows any origin).
	CORSOrigins []string `yaml:"cors_origins"`

	// SendRatePerMinute limits sends per API key (burst 20).
	SendRatePerMinute int `yaml:"send_rate_per_minute"`
}

// DefaultPath is where the config file is looked for when none is given.
func DefaultPath() string {
	return filepath.Join(homeDir(), ".config", "imsgd", "config.yml")
}

// Defaults returns a Config populated with default values.
func Defaults() Config {
	return Config{
		Port:        8642,
		DataDir:     filepath.Join(homeDir(), "Library", "Application Support", "imsgd"),
		LogLevel:    "info",
		Contacts:    Contacts{Enabled: true},
		MessagesDB:  filepath.Join(homeDir(), "Library", "Messages", "chat.db"),
		UploadMaxMB: 100,
		CacheMaxMB:  2048,

		SendRatePerMinute: 60,
	}
}

// Load reads the config file at path (if it exists) over the defaults, then
// applies IMSGD_* environment overrides. A missing file is not an error when
// path is the default path.
func Load(path string) (Config, error) {
	cfg := Defaults()
	explicit := path != ""
	if !explicit {
		path = DefaultPath()
	}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("parse %s: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist) && !explicit:
		// Defaults only.
	default:
		return cfg, fmt.Errorf("read config: %w", err)
	}

	if err := applyEnv(&cfg); err != nil {
		return cfg, err
	}
	cfg.DataDir = expandHome(cfg.DataDir)
	cfg.MessagesDB = expandHome(cfg.MessagesDB)
	cfg.LogFile = expandHome(cfg.LogFile)
	cfg.TLS.CertFile = expandHome(cfg.TLS.CertFile)
	cfg.TLS.KeyFile = expandHome(cfg.TLS.KeyFile)
	return cfg, nil
}

func applyEnv(cfg *Config) error {
	if v, ok := os.LookupEnv("IMSGD_LISTEN"); ok {
		cfg.Listen = v
	}
	if v, ok := os.LookupEnv("IMSGD_PORT"); ok {
		p, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("IMSGD_PORT: %w", err)
		}
		cfg.Port = p
	}
	if v, ok := os.LookupEnv("IMSGD_DATA_DIR"); ok {
		cfg.DataDir = v
	}
	if v, ok := os.LookupEnv("IMSGD_LOG_LEVEL"); ok {
		cfg.LogLevel = v
	}
	if v, ok := os.LookupEnv("IMSGD_LOG_FILE"); ok {
		cfg.LogFile = v
	}
	if v, ok := os.LookupEnv("IMSGD_MESSAGES_DB"); ok {
		cfg.MessagesDB = v
	}
	// IMSGD_API_KEY adds a single key named "env"; handy for quick testing.
	if v, ok := os.LookupEnv("IMSGD_API_KEY"); ok && v != "" {
		cfg.APIKeys = append(cfg.APIKeys, APIKey{Name: "env", Key: v})
	}
	return nil
}

// ValidateForServe checks settings required to run the HTTP server.
func (c Config) ValidateForServe() error {
	var errs []error
	if c.Port <= 0 || c.Port > 65535 {
		errs = append(errs, fmt.Errorf("port %d out of range", c.Port))
	}
	if len(c.APIKeys) == 0 {
		errs = append(errs, errors.New("at least one api_keys entry is required"))
	}
	seen := map[string]bool{}
	for i, k := range c.APIKeys {
		if k.Name == "" {
			errs = append(errs, fmt.Errorf("api_keys[%d]: name is required", i))
		}
		if len(k.Key) < 16 {
			errs = append(errs, fmt.Errorf("api_keys[%d] (%s): key must be at least 16 characters", i, k.Name))
		}
		if seen[k.Name] {
			errs = append(errs, fmt.Errorf("api_keys[%d]: duplicate name %q", i, k.Name))
		}
		seen[k.Name] = true
	}
	if (c.TLS.CertFile == "") != (c.TLS.KeyFile == "") {
		errs = append(errs, errors.New("tls: cert_file and key_file must be set together"))
	}
	return errors.Join(errs...)
}

func homeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return h
}

func expandHome(p string) string {
	if p == "~" {
		return homeDir()
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(homeDir(), p[2:])
	}
	return p
}
