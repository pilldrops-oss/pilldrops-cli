package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A pipe reaches the remote command; an empty or terminal stdin sends none.
func TestExecSendsPipedStdin(t *testing.T) {
	var got map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes/sb_1/exec", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = nil
		_ = json.Unmarshal(b, &got)
		_, _ = w.Write([]byte(`{"stdout":"ok","exit_code":0}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	cpEnv(t, srv.URL)

	stdin = strings.NewReader(`{"a":1}`)
	defer func() { stdin = os.Stdin }()
	var out, errb bytes.Buffer
	if code := Run([]string{"exec", "sb_1", "--", "cat"}, &out, &errb); code != 0 {
		t.Fatalf("exec = %d %s", code, errb.String())
	}
	raw, _ := got["stdin"].(string)
	dec, _ := base64.StdEncoding.DecodeString(raw)
	if string(dec) != `{"a":1}` {
		t.Fatalf("stdin sent = %q", dec)
	}

	stdin = strings.NewReader("")
	if code := Run([]string{"exec", "sb_1", "--", "cat"}, &out, &errb); code != 0 {
		t.Fatalf("exec = %d", code)
	}
	if _, ok := got["stdin"]; ok {
		t.Errorf("empty stdin must not be sent: %v", got)
	}
}

func fakeExecPortal(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes/sb_1/exec", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Cmd string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch body.Cmd {
		case "echo hi":
			w.Write([]byte(`{"stdout":"hi\n","stderr":"","exit_code":0,"truncated":false}`))
		case "boom":
			w.Write([]byte(`{"stdout":"","stderr":"kaboom\n","exit_code":7,"truncated":false}`))
		default:
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"cmd required"}`))
		}
	})
	return httptest.NewServer(mux)
}

func TestExecStreamsAndPropagatesExitCode(t *testing.T) {
	srv := fakeExecPortal(t)
	defer srv.Close()
	t.Setenv("SANDFORGE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("SANDFORGE_API_URL", srv.URL)
	t.Setenv("SANDFORGE_API_KEY", "sfc_good")

	var out, errb bytes.Buffer
	if code := Run([]string{"exec", "sb_1", "--", "echo", "hi"}, &out, &errb); code != 0 {
		t.Fatalf("exec = %d", code)
	}
	if out.String() != "hi\n" {
		t.Fatalf("stdout = %q, want %q", out.String(), "hi\n")
	}

	out.Reset()
	errb.Reset()
	if code := Run([]string{"exec", "sb_1", "--", "boom"}, &out, &errb); code != 7 {
		t.Fatalf("remote exit 7 should become CLI exit 7, got %d", code)
	}
	if !strings.Contains(errb.String(), "kaboom") {
		t.Fatalf("stderr = %q, want remote stderr", errb.String())
	}
}

func TestExecJSONMode(t *testing.T) {
	srv := fakeExecPortal(t)
	defer srv.Close()
	t.Setenv("SANDFORGE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("SANDFORGE_API_URL", srv.URL)
	t.Setenv("SANDFORGE_API_KEY", "sfc_good")

	var out, errb bytes.Buffer
	if code := Run([]string{"--json", "exec", "sb_1", "--", "boom"}, &out, &errb); code != 7 {
		t.Fatalf("json exec = %d, want 7", code)
	}
	var res map[string]any
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("non-JSON: %v\n%s", err, out.String())
	}
	if res["exit_code"] != float64(7) {
		t.Fatalf("exit_code = %v", res["exit_code"])
	}
}

func TestExecUsageErrors(t *testing.T) {
	t.Setenv("SANDFORGE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("SANDFORGE_API_KEY", "sfc_good")
	t.Setenv("SANDFORGE_API_URL", "http://unused.invalid")
	var out, errb bytes.Buffer
	if code := Run([]string{"exec", "sb_1"}, &out, &errb); code != ExitUsage {
		t.Fatalf("exec without -- cmd = %d, want %d", code, ExitUsage)
	}
}

// TestExecEnvFlag is the S7 CLI half: `exec --env K=V` sends per-command env as
// a JSON object the node applies (E2B SDK envs= parity).
func TestExecEnvFlag(t *testing.T) {
	var gotEnv map[string]string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes/sb_1/exec", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Cmd string            `json:"cmd"`
			Env map[string]string `json:"env"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotEnv = body.Env
		w.Write([]byte(`{"stdout":"ok\n","stderr":"","exit_code":0,"truncated":false}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("SANDFORGE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("SANDFORGE_API_URL", srv.URL)
	t.Setenv("SANDFORGE_API_KEY", "sfc_good")

	var out, errb bytes.Buffer
	code := Run([]string{"exec", "sb_1", "--env", "FOO=bar", "--env", "BAZ=qux", "--", "true"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exec --env = %d (%s)", code, errb.String())
	}
	if gotEnv["FOO"] != "bar" || gotEnv["BAZ"] != "qux" {
		t.Fatalf("node received env %v, want FOO=bar BAZ=qux", gotEnv)
	}
}

func TestExecEnvFlagRejectsBadFormat(t *testing.T) {
	t.Setenv("SANDFORGE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("SANDFORGE_API_KEY", "sfc_good")
	t.Setenv("SANDFORGE_API_URL", "http://unused.invalid")
	var out, errb bytes.Buffer
	if code := Run([]string{"exec", "sb_1", "--env", "NOEQUALS", "--", "true"}, &out, &errb); code != ExitUsage {
		t.Fatalf("bad --env = %d, want ExitUsage", code)
	}
}
