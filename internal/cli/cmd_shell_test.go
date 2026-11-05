package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestShellSplit(t *testing.T) {
	for _, tc := range []struct {
		line string
		want []string
	}{
		{"", nil},
		{"   \t ", nil},
		{"sandbox ls", []string{"sandbox", "ls"}},
		{"  a   b\tc ", []string{"a", "b", "c"}},
		{`exec sb -- bash -c 'cat /a | wc -l'`, []string{"exec", "sb", "--", "bash", "-c", "cat /a | wc -l"}},
		{`agent ask sb "fix \"this\" bug"`, []string{"agent", "ask", "sb", `fix "this" bug`}},
		{`a\ b`, []string{"a b"}},
		{`'a\b'`, []string{`a\b`}},
		{`"a\b"`, []string{`a\b`}},
		{`"a\\b"`, []string{`a\b`}},
		{`"$HOME"`, []string{"$HOME"}},
		{`x="1 2"y`, []string{"x=1 2y"}},
		{`"" ''`, []string{"", ""}},
		{`é "ü"`, []string{"é", "ü"}},
	} {
		got, err := shellSplit(tc.line)
		if err != nil {
			t.Errorf("%q: %v", tc.line, err)
		} else if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q = %q, want %q", tc.line, got, tc.want)
		}
	}
	for _, line := range []string{`"open`, `'open`, `trailing\`} {
		if _, err := shellSplit(line); err == nil {
			t.Errorf("%q should not split", line)
		}
	}
}

// runShell drives the loop with Run itself standing in for the child process,
// so what is under test is that a line reaches the same command table.
func runShell(t *testing.T, input string) (string, string, [][]string) {
	t.Helper()
	var out, errb bytes.Buffer
	var ran [][]string
	var atPrompt atomic.Bool
	code := shellLoop(strings.NewReader(input), &out, &errb, func(args []string) int {
		ran = append(ran, args)
		return Run(args, &out, &errb)
	}, &atPrompt)
	if code != ExitOK {
		t.Fatalf("shell exited %d", code)
	}
	return out.String(), errb.String(), ran
}

func TestShellLoopDispatchesAndSurvivesFailures(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sandboxes", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"sandboxes":[{"id":"sb_1","template":"t","status":"running"}]}`))
	})
	mux.HandleFunc("GET /v1/sandboxes/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":"no such sandbox"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("SANDFORGE_CONFIG", t.TempDir()+"/config.json")
	t.Setenv("SANDFORGE_API_URL", srv.URL)
	t.Setenv("SANDFORGE_API_KEY", "sfc_good")

	out, errS, ran := runShell(t, strings.Join([]string{
		"sandbox get nope", // fails, and the shell goes on
		"",
		`bad "quote`,
		"frobnicate",
		"shell",
		"--json sandbox ls",
		"exit",
		"sandbox ls", // past exit: never read
	}, "\n"))

	want := [][]string{{"sandbox", "get", "nope"}, {"--json", "sandbox", "ls"}}
	if !reflect.DeepEqual(ran, want) {
		t.Fatalf("ran %q, want %q", ran, want)
	}
	for _, s := range []string{"no such sandbox", "unterminated", `unknown command "frobnicate"`, "already in the shell"} {
		if !strings.Contains(errS, s) {
			t.Errorf("stderr %q lacks %q", errS, s)
		}
	}
	if !strings.Contains(out, shellPrompt) || !strings.Contains(out, `"id": "sb_1"`) {
		t.Errorf("stdout = %q", out)
	}
}

func TestShellLoopLeaving(t *testing.T) {
	for _, input := range []string{"exit\nversion\n", "quit\nversion\n", ""} {
		if _, _, ran := runShell(t, input); len(ran) != 0 {
			t.Errorf("%q ran %q", input, ran)
		}
	}
	out, _, _ := runShell(t, "help\n")
	if !strings.Contains(out, "usage: pilldrops") || !strings.Contains(out, "Ctrl-D") {
		t.Errorf("help = %q", out)
	}
}

// Without a terminal the old contract stands; prompt_pty_test covers the other side.
func TestShellNeedsATerminal(t *testing.T) {
	for _, args := range [][]string{{"shell"}, {"--json", "shell"}} {
		code, _, errS := runCLI(t, "http://unused.invalid", args...)
		if code != ExitUsage || !strings.Contains(errS, shellUsage) {
			t.Errorf("%v = %d, stderr %q", args, code, errS)
		}
	}
}

// `shell <id>` was connect's alias, and scripts may still type it.
func TestShellWithAnIDStillConnects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":"no such sandbox"}`))
	}))
	defer srv.Close()
	if code, _, errS := authedCLI(t, srv, "shell", "sb_1"); code != ExitAPI || !strings.Contains(errS, "no such sandbox") {
		t.Fatalf("exit %d stderr %q", code, errS)
	}
}

func TestCatalogKeepsAgentsOutOfTheShell(t *testing.T) {
	_, out, _ := runCLI(t, "", "--json", "ai")
	var catalog struct {
		Catalog  string   `json:"catalog"`
		Commands []string `json:"commands"`
	}
	if err := json.Unmarshal([]byte(out), &catalog); err != nil {
		t.Fatal(err)
	}
	for _, p := range catalog.Commands {
		if p == "shell" {
			t.Fatal("the shell is advertised to agents as a command")
		}
	}
	if !strings.Contains(catalog.Catalog, "Do not use pilldrops shell") {
		t.Fatal("the catalog does not warn agents off the shell")
	}
	if strings.Contains(catalog.Catalog, "a pilldrops> prompt for every command") {
		t.Fatal("the shell's help block is in the catalog")
	}
	var help strings.Builder
	usage(&help)
	if !strings.Contains(help.String(), "a pilldrops> prompt") {
		t.Fatal("the shell is missing from help")
	}
}
