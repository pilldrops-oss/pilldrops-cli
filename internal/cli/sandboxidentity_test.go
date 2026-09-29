package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeIdentity lays out what a sandbox holds at /run/pilldrops.
func writeIdentity(t *testing.T, apiURL, token string) {
	t.Helper()
	dir := t.TempDir()
	// The node writes the token with a trailing newline.
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := "PILLDROPS_API_URL=" + apiURL + "\nPD_SANDBOX_ID=sb_ab12cd\nPD_SANDBOX_DOMAIN=\n"
	if err := os.WriteFile(filepath.Join(dir, "env"), []byte(env), 0o644); err != nil {
		t.Fatal(err)
	}
	old := identityDir
	identityDir = dir
	t.Cleanup(func() { identityDir = old })
}

// Inside a sandbox, with no environment variable and no config file, the CLI
// works off the credential the platform already put on disk.
func TestSandboxIdentityIsUsedWithNoEnvAndNoConfig(t *testing.T) {
	srv := fakePlatform(t, `{"org":"acme","principal":"sandbox","sandbox_id":"sb_ab12cd"}`)
	defer srv.Close()

	t.Setenv("SANDFORGE_CONFIG", filepath.Join(t.TempDir(), "config.json")) // no file there
	t.Setenv("SANDFORGE_API_KEY", "")
	t.Setenv("SANDFORGE_API_URL", "")
	t.Setenv("PILLDROPS_API_KEY", "")
	t.Setenv("PILLDROPS_API_URL", "")
	writeIdentity(t, srv.URL, "sf_good")

	var out, errb bytes.Buffer
	if code := Run([]string{"whoami"}, &out, &errb); code != ExitOK {
		t.Fatalf("whoami = %d, stderr = %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "sandbox sb_ab12cd") {
		t.Fatalf("whoami said %q, want the sandbox principal", out.String())
	}
}

// An environment variable still wins: an agent that exports a key inside a
// sandbox means to use that key.
func TestEnvKeyBeatsSandboxIdentity(t *testing.T) {
	t.Setenv("SANDFORGE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("SANDFORGE_API_KEY", "")
	t.Setenv("PILLDROPS_API_KEY", "sf_from_env")
	writeIdentity(t, "http://identity.invalid", "sf_from_disk")

	if _, key := resolveCreds(""); key != "sf_from_env" {
		t.Fatalf("key = %q, want the environment's", key)
	}
}

// …and the identity beats the config file, which is the stale one when both
// exist: the config was written on someone's laptop, the identity is this
// sandbox's own.
func TestSandboxIdentityBeatsConfigFile(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("SANDFORGE_CONFIG", cfgPath)
	t.Setenv("SANDFORGE_API_KEY", "")
	t.Setenv("PILLDROPS_API_KEY", "")
	t.Setenv("SANDFORGE_API_URL", "")
	t.Setenv("PILLDROPS_API_URL", "")
	if err := SaveConfig(Config{APIURL: "http://config.invalid", Key: "sf_from_config"}); err != nil {
		t.Fatal(err)
	}
	writeIdentity(t, "http://identity.invalid", "sf_from_disk")

	url, key := resolveCreds("")
	if key != "sf_from_disk" || url != "http://identity.invalid" {
		t.Fatalf("resolved %s / %q, want the sandbox's own identity", url, key)
	}
}

// No identity file — the ordinary case everywhere but inside a sandbox.
func TestNoIdentityFileChangesNothing(t *testing.T) {
	t.Setenv("SANDFORGE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("SANDFORGE_API_KEY", "")
	t.Setenv("PILLDROPS_API_KEY", "")
	old := identityDir
	identityDir = filepath.Join(t.TempDir(), "absent")
	t.Cleanup(func() { identityDir = old })

	if _, key := resolveCreds(""); key != "" {
		t.Fatalf("key = %q, want none", key)
	}
}
