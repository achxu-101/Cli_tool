package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

const currentVersion = "1"

// UserBinary holds a user-taught upgrade method for an unknown binary.
type UserBinary struct {
	Name       string `json:"name"`
	Method     string `json:"method"`
	GithubRepo string `json:"github_repo,omitempty"`
	AptPackage string `json:"apt_package,omitempty"`
	ScriptURL  string `json:"script_url,omitempty"`
	BinaryPath string `json:"binary_path"`
	Overridden bool   `json:"overridden"` // true if overriding a known default
	AddedAt    string `json:"added_at"`
}

// Config is the contents of ~/.config/upgrador/known.json.
type Config struct {
	Version  string                `json:"version"`
	Binaries map[string]UserBinary `json:"binaries"`
}

func configPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "upgrador", "known.json"), nil
}

// Load reads the config file. Returns an empty config if the file does not exist.
func Load() (*Config, error) {
	path, err := configPath()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Config{Version: currentVersion, Binaries: make(map[string]UserBinary)}, nil
	}
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if cfg.Binaries == nil {
		cfg.Binaries = make(map[string]UserBinary)
	}
	return &cfg, nil
}

// Save writes the config to disk, creating parent directories if needed.
func (c *Config) Save() error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// GetBinary returns the user-defined entry for the named binary, if present.
func (c *Config) GetBinary(name string) (*UserBinary, bool) {
	b, ok := c.Binaries[name]
	if !ok {
		return nil, false
	}
	return &b, true
}

// SetBinary stores the binary entry and immediately persists the config.
func (c *Config) SetBinary(b UserBinary) error {
	if b.AddedAt == "" {
		b.AddedAt = time.Now().UTC().Format(time.RFC3339)
	}
	c.Binaries[b.Name] = b
	return c.Save()
}

// RemoveBinary deletes the named binary entry and immediately persists the config.
func (c *Config) RemoveBinary(name string) error {
	delete(c.Binaries, name)
	return c.Save()
}
