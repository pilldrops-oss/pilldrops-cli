package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// The AI-native create flags (--image, --start, --port, --env, --storage) must
// reach the API so an agent can launch a custom image, run an app server, and
// get its preview URL — all from the CLI.
func TestSandboxCreateAdvancedFlags(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/v1/sandboxes" {
			_ = json.NewDecoder(r.Body).Decode(&got)
			w.WriteHeader(201)
			w.Write([]byte(`{"id":"sb_9","template":"base","status":"running","url":"https://8080-sb_9.example.com"}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()

	t.Setenv("SANDFORGE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("SANDFORGE_API_URL", srv.URL)
	t.Setenv("SANDFORGE_API_KEY", "sfc_good")

	var out, errb bytes.Buffer
	code := Run([]string{
		"sandbox", "create",
		"--image", "ghcr.io/me/app:1.2",
		"--start", "npm run dev",
		"--port", "3000", "--port", "8080",
		"--env", "NODE_ENV=production", "--env", "PORT=3000",
		"--storage", "4096",
	}, &out, &errb)
	if code != ExitOK {
		t.Fatalf("create = %d stderr=%s", code, errb.String())
	}

	if got["image"] != "ghcr.io/me/app:1.2" {
		t.Errorf("image not forwarded: %v", got["image"])
	}
	if got["start"] != "npm run dev" {
		t.Errorf("start not forwarded: %v", got["start"])
	}
	if got["storage_mb"] != float64(4096) {
		t.Errorf("storage_mb not forwarded: %v", got["storage_mb"])
	}
	ports, _ := got["ports"].([]any)
	if len(ports) != 2 || ports[0] != float64(3000) || ports[1] != float64(8080) {
		t.Errorf("ports not forwarded: %v", got["ports"])
	}
	env, _ := got["env"].([]any)
	if len(env) != 2 || env[0] != "NODE_ENV=production" {
		t.Errorf("env not forwarded: %v", got["env"])
	}
	// The preview URL is surfaced so an agent can hit the running app.
	if !strings.Contains(out.String(), "preview: https://8080-sb_9.example.com") {
		t.Errorf("preview URL not shown: %q", out.String())
	}
}
