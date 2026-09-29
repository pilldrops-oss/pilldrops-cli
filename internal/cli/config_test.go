package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigRoundtripAndPerms(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("SANDFORGE_CONFIG", path)

	if c, err := LoadConfig(); err != nil || c.Key != "" {
		t.Fatalf("missing file should load zero config, got %+v err=%v", c, err)
	}

	in := Config{APIURL: "https://portal.example", Key: "sfc_abc", Email: "a@b.c"}
	if err := SaveConfig(in); err != nil {
		t.Fatalf("save: %v", err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
	out, err := LoadConfig()
	if err != nil || out != in {
		t.Fatalf("load = %+v err=%v, want %+v", out, err, in)
	}
	if err := DeleteConfig(); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if c, _ := LoadConfig(); c.Key != "" {
		t.Fatal("config survived delete")
	}
}

func TestCredentialResolutionOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("SANDFORGE_CONFIG", path)
	_ = SaveConfig(Config{APIURL: "https://file.example", Key: "sfc_file"})

	// Env beats file.
	t.Setenv("SANDFORGE_API_KEY", "sfc_env")
	t.Setenv("SANDFORGE_API_URL", "https://env.example")
	url, key := resolveCreds("")
	if key != "sfc_env" || url != "https://env.example" {
		t.Fatalf("env should win: url=%q key=%q", url, key)
	}

	// Flag beats env for the URL.
	if url, _ := resolveCreds("https://flag.example"); url != "https://flag.example" {
		t.Fatalf("flag should win: %q", url)
	}

	// File is the fallback.
	t.Setenv("SANDFORGE_API_KEY", "")
	t.Setenv("SANDFORGE_API_URL", "")
	url, key = resolveCreds("")
	if key != "sfc_file" || url != "https://file.example" {
		t.Fatalf("file fallback: url=%q key=%q", url, key)
	}
}
