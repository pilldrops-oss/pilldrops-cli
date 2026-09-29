package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnvSetListRm(t *testing.T) {
	var posted []map[string]any
	var deleted string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/secrets", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		posted = append(posted, m)
		kind := "env"
		if m["secret"] == true {
			kind = "secret"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"name": m["name"], "kind": kind, "auto_inject": m["auto_inject"], "last4": "cdef"})
	})
	mux.HandleFunc("GET /v1/secrets", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"name":"MODEL","kind":"env","auto_inject":true,"last4":"-4.1","value":"gpt-4.1"},
			{"name":"TOKEN","kind":"secret","auto_inject":false,"last4":"cdef"}]`))
	})
	mux.HandleFunc("DELETE /v1/secrets/{name}", func(w http.ResponseWriter, r *http.Request) {
		deleted = r.PathValue("name")
		w.WriteHeader(204)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("SANDFORGE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("SANDFORGE_API_URL", srv.URL)
	t.Setenv("SANDFORGE_API_KEY", "sfc_good")

	var out, errb bytes.Buffer
	if code := Run([]string{"env", "set", "TOKEN", "abcdef", "--secret", "--auto"}, &out, &errb); code != ExitOK {
		t.Fatalf("set = %d %s", code, errb.String())
	}
	if code := Run([]string{"env", "set", "MODEL", "gpt-4.1"}, &out, &errb); code != ExitOK {
		t.Fatalf("set = %d %s", code, errb.String())
	}
	if len(posted) != 2 {
		t.Fatalf("posted %d bodies", len(posted))
	}
	if posted[0]["name"] != "TOKEN" || posted[0]["value"] != "abcdef" || posted[0]["secret"] != true || posted[0]["auto_inject"] != true {
		t.Errorf("secret+auto body = %v", posted[0])
	}
	if posted[1]["secret"] != false || posted[1]["auto_inject"] != false {
		t.Errorf("plain body = %v", posted[1])
	}

	out.Reset()
	if code := Run([]string{"env", "list"}, &out, &errb); code != ExitOK {
		t.Fatalf("list = %d %s", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "(sealed)") || strings.Contains(got, "abcdef") {
		t.Errorf("secret must list as (sealed): %s", got)
	}
	if !strings.Contains(got, "gpt-4.1") || !strings.Contains(got, "yes") {
		t.Errorf("env kind must list its value and auto column: %s", got)
	}

	if code := Run([]string{"env", "rm", "TOKEN"}, &out, &errb); code != ExitOK || deleted != "TOKEN" {
		t.Errorf("rm = %d deleted=%q", code, deleted)
	}
	if code := Run([]string{"env", "set", "ONLY"}, &out, &errb); code != ExitUsage {
		t.Errorf("set with one arg = %d, want usage", code)
	}
}
