// Package config is the single source of truth for daemon settings.
//
// The QML plugin never reads or writes the config file directly; it shells out
// to `omarchy-connect`, which comes through here. Two writers to one JSON file,
// one of them a shell that hot-reloads on save, is a race nobody needs.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// DefaultPort is the tailnet listener port. 7433 spells "SESS" on a phone
// keypad, which is the only justification offered.
const DefaultPort = 7433

// Config is the on-disk daemon configuration.
type Config struct {
	// Port is the tailnet listener port.
	Port int `json:"port"`

	// LAN configures the tier-2 listener: plain HTTP on a LAN address, for a
	// phone with no Tailscale installed. Off unless explicitly enabled.
	LAN LAN `json:"lan"`
}

// LAN is the tier-2 listener config. It is HTTP-only and always will be: the
// tailnet certificate carries a single SAN for the MagicDNS name, so nothing
// will ever make it valid for a LAN address.
type LAN struct {
	Enabled bool `json:"enabled"`
	Port    int  `json:"port"`
}

// Default returns the configuration used when none has been written yet.
func Default() Config {
	return Config{
		Port: DefaultPort,
		LAN:  LAN{Enabled: false, Port: DefaultPort},
	}
}

// Dir returns the directory holding the daemon's configuration and state.
func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locating user config dir: %w", err)
	}
	return filepath.Join(base, "omarchy", "connect"), nil
}

// Path returns the full path of the config file.
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// Load reads the config, falling back to defaults when the file does not exist.
//
// A missing file is not an error: the daemon is expected to run correctly
// before anyone has opened the settings panel. A malformed file *is* an error,
// because silently reverting someone's settings to defaults is worse than
// refusing to start and saying why.
func Load() (Config, error) {
	path, err := Path()
	if err != nil {
		return Config{}, err
	}

	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("reading %s: %w", path, err)
	}

	// Start from defaults so a config written by an older version keeps working
	// when new fields appear: absent keys keep their default rather than
	// becoming a zero value that means something different.
	cfg := Default()
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return Config{}, fmt.Errorf("in %s: %w", path, err)
	}
	return cfg, nil
}

// Save writes the config atomically.
func Save(cfg Config) error {
	if err := cfg.validate(); err != nil {
		return err
	}

	dir, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding config: %w", err)
	}
	raw = append(raw, '\n')

	// Write-then-rename: a crash mid-write must not leave a truncated config
	// that fails to parse and takes the daemon down on next start.
	tmp, err := os.CreateTemp(dir, "config.*.json")
	if err != nil {
		return fmt.Errorf("creating temp config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("writing temp config: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("setting config permissions: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp config: %w", err)
	}

	path := filepath.Join(dir, "config.json")
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("installing config: %w", err)
	}
	return nil
}

func (c Config) validate() error {
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("port %d out of range", c.Port)
	}
	if c.LAN.Enabled && (c.LAN.Port < 1 || c.LAN.Port > 65535) {
		return fmt.Errorf("lan.port %d out of range", c.LAN.Port)
	}
	return nil
}
