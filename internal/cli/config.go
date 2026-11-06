// Package cli implements the sandforge customer CLI: login, sandbox lifecycle,
// exec, logs, files, and inspection. Output is AI-operable: --json everywhere, stable exit codes, no prompts without a TTY.
package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Config is the stored credential (~/.sandforge/config.json, 0600).
type Config struct {
	APIURL string `json:"api_url"`
	Key    string `json:"key"`
	Email  string `json:"email"`
	// A team session stands for a person, unlike Key, and usually a different server.
	TeamSession string `json:"team_session,omitempty"`
	TeamPortal  string `json:"team_portal,omitempty"`
	TeamEmail   string `json:"team_email,omitempty"`
}

// configPath honours SANDFORGE_CONFIG (tests, unusual setups), defaulting to
// ~/.sandforge/config.json.
func configPath() string {
	if p := os.Getenv("SANDFORGE_CONFIG"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".sandforge/config.json"
	}
	return filepath.Join(home, ".sandforge", "config.json")
}

// LoadConfig reads the stored config; a missing file is a zero Config, not an
// error (the "not logged in" state).
func LoadConfig() (Config, error) {
	var c Config
	raw, err := os.ReadFile(configPath())
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(raw, &c)
}

// SaveConfig writes the config with owner-only permissions.
func SaveConfig(c Config) error {
	p := configPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(p, raw, 0o600)
}

// DeleteConfig removes the stored credential (logout). Missing file is fine.
func DeleteConfig() error {
	err := os.Remove(configPath())
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// identityDir is where a sandbox holds its own platform credential: token
// (0600) and env (PILLDROPS_API_URL, PD_SANDBOX_ID, PD_SANDBOX_DOMAIN). Every
// base template ships this CLI, so an agent working inside a sandbox has a
// credential on disk and should not have to be handed one. A var so tests can
// point it somewhere writable.
var identityDir = "/run/pilldrops"

// sandboxIdentity reads that credential, or returns empty when there is none —
// which is the case everywhere except inside a sandbox.
func sandboxIdentity() (apiURL, token string) {
	raw, err := os.ReadFile(filepath.Join(identityDir, "token"))
	if err != nil {
		return "", ""
	}
	token = strings.TrimSpace(string(raw))
	if token == "" {
		return "", ""
	}
	// envd layers this file into every exec and login shell, so the URL is
	// usually already in the environment; read it anyway for the processes it
	// did not layer into.
	env, err := os.ReadFile(filepath.Join(identityDir, "env"))
	if err != nil {
		return "", token
	}
	for _, line := range strings.Split(string(env), "\n") {
		if v, ok := strings.CutPrefix(line, "PILLDROPS_API_URL="); ok {
			apiURL = strings.TrimSpace(v)
		}
	}
	return apiURL, token
}

// resolveCreds resolves the API URL and key, in this order:
//
//	--api-url flag > PILLDROPS_* env > SANDFORGE_* env > /run/pilldrops
//	(inside a sandbox) > ~/.sandforge/config.json
//
// PILLDROPS_* is the canonical env name; SANDFORGE_* stays accepted so existing
// setups keep working.
func resolveCreds(flagURL string) (apiURL, key string) {
	cfg, _ := LoadConfig()
	apiURL, key = cfg.APIURL, cfg.Key
	if sbURL, sbKey := sandboxIdentity(); sbKey != "" {
		key = sbKey
		if sbURL != "" {
			apiURL = sbURL
		}
	}
	if v := os.Getenv("SANDFORGE_API_URL"); v != "" {
		apiURL = v
	}
	if v := os.Getenv("PILLDROPS_API_URL"); v != "" {
		apiURL = v
	}
	if flagURL != "" {
		apiURL = flagURL
	}
	if v := os.Getenv("SANDFORGE_API_KEY"); v != "" {
		key = v
	}
	if v := os.Getenv("PILLDROPS_API_KEY"); v != "" {
		key = v
	}
	return apiURL, key
}
