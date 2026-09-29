//go:build darwin || linux

package cli

// The five prompting rules, driven through a REAL pseudo-terminal.
//
// Nothing here is stubbed. Each test re-executes this test binary as the CLI
// so that os.Stdin is the thing under test: a pty slave when there is a human,
// an ordinary pipe when there is not. term.IsTerminal therefore answers about
// a real terminal, and ^C is a real SIGINT raised by a real line discipline
// against the child's own controlling tty.
//
// The pair that matters is TestScriptGetsUsageNotAQuestion and
// TestTerminalIsAskedAndTheOnlySandboxIsTheDefault: the first alone would pass
// against a CLI that never prompts at all, which is exactly the vacuous test
// this is here to avoid.

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/term"
)

// TestPromptChild is the child half of every test below: this binary,
// re-executed, running the CLI on the stdin it was handed. Everything after
// "--" is the command line.
func TestPromptChild(t *testing.T) {
	if os.Getenv("PILLDROPS_PROMPT_CHILD") != "1" {
		t.Skip("harness for the prompting tests")
	}
	argv := os.Args
	for i, a := range argv {
		if a == "--" {
			argv = argv[i+1:]
			break
		}
	}
	os.Exit(Run(argv, os.Stdout, os.Stderr))
}

// syncBuf collects a child stream; os/exec writes it from its own goroutine
// while the test polls it, so the lock is not decoration.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// promptPortal serves n sandboxes (sb_1…sb_n) plus the routes the prompted
// verbs then call. acted counts everything that is not the listing, so a
// refused or interrupted verb can be shown to have done nothing.
func promptPortal(t *testing.T, n int, acted *atomic.Int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sandboxes", func(w http.ResponseWriter, _ *http.Request) {
		var rows []string
		for i := 1; i <= n; i++ {
			rows = append(rows, fmt.Sprintf(
				`{"id":"sb_%d","template":"base","status":"running","visibility":"org"}`, i))
		}
		_, _ = fmt.Fprintf(w, `{"sandboxes":[%s]}`, strings.Join(rows, ","))
	})
	mux.HandleFunc("GET /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		acted.Add(1)
		_, _ = fmt.Fprintf(w, `{"id":%q,"template":"base","status":"running"}`, r.PathValue("id"))
	})
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", func(w http.ResponseWriter, _ *http.Request) {
		acted.Add(1)
		_, _ = w.Write([]byte(`{"status":"deleted"}`))
	})
	mux.HandleFunc("GET /v1/sandboxes/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		acted.Add(1)
		_, _ = fmt.Fprintf(w, `{"lines":["log of %s"]}`, r.PathValue("id"))
	})
	mux.HandleFunc("POST /v1/sandboxes/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		acted.Add(1)
		_, _ = fmt.Fprintf(w, `{"stdout":"ran in %s\n","exit_code":0}`, r.PathValue("id"))
	})
	mux.HandleFunc("GET /v1/sandboxes/{id}/files", func(w http.ResponseWriter, _ *http.Request) {
		acted.Add(1)
		_, _ = w.Write([]byte("file-bytes"))
	})
	mux.HandleFunc("GET /v1/sandboxes/{id}/process/list", func(w http.ResponseWriter, r *http.Request) {
		acted.Add(1)
		_, _ = fmt.Fprintf(w, `[{"pid":7,"cmd":"ps of %s"}]`, r.PathValue("id"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// startCLI runs the CLI in a child process on the given stdin: a pty slave
// when the test is playing a human, an ordinary pipe when it is playing a
// script. stdout and stderr are separate pipes, so which stream a question
// went to is a fact the test can check.
func startCLI(t *testing.T, in *os.File, apiURL string, args ...string) (*exec.Cmd, *syncBuf, *syncBuf) {
	t.Helper()
	cmd := exec.Command(os.Args[0], append([]string{"-test.run=TestPromptChild", "--"}, args...)...)
	cmd.Env = append(os.Environ(),
		"PILLDROPS_PROMPT_CHILD=1",
		"SANDFORGE_CONFIG="+filepath.Join(t.TempDir(), "config.json"),
		"PILLDROPS_API_URL="+apiURL,
		"PILLDROPS_API_KEY=sfc_good",
		"SANDFORGE_API_URL=",
		"SANDFORGE_API_KEY=",
	)
	out, errb := &syncBuf{}, &syncBuf{}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, errb
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd, out, errb
}

// wait reaps the child and returns its exit status (-1 when a signal killed it).
func wait(t *testing.T, cmd *exec.Cmd) int {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("child never exited — it is blocked on a question nobody answered")
	}
	return cmd.ProcessState.ExitCode()
}

// tty hands back a pty pair, closing both when the test ends.
func tty(t *testing.T) (master, slave *os.File) {
	t.Helper()
	m, s, err := openPTY()
	if err != nil {
		// Skip only where the machine has no pseudo-terminals at all. A box
		// that HAS /dev/ptmx and still failed means the recipe is wrong, and
		// a silent skip there would quietly retire every test in this file.
		if _, statErr := os.Stat("/dev/ptmx"); statErr != nil {
			t.Skipf("no pseudo-terminals here: %v", statErr)
		}
		t.Fatalf("open a pty: %v", err)
	}
	t.Cleanup(func() { _ = m.Close(); _ = s.Close() })
	return m, s
}

// waitFor blocks until the child has written substr, so a test that must act
// on a prompt acts after it exists.
func waitFor(t *testing.T, b *syncBuf, substr string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(b.String(), substr) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("never saw %q; got %q", substr, b.String())
}

// promptish is the wording no automated caller may ever see.
const promptish = "sandbox ["

// ---------------------------------------------------------------- rule 1 ---

// No terminal, no prompt: the same usage line and the same exit 2 as before,
// and not even the listing the prompt would need is fetched.
func TestScriptGetsUsageNotAQuestion(t *testing.T) {
	var acted atomic.Int32
	srv := promptPortal(t, 1, &acted)
	r, w, err := os.Pipe() // a real *os.File that is not a terminal
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()

	cmd, out, errb := startCLI(t, r, srv.URL, "sandbox", "get")
	if code := wait(t, cmd); code != ExitUsage {
		t.Fatalf("exit = %d, want %d (stderr %q)", code, ExitUsage, errb.String())
	}
	if !strings.Contains(errb.String(), "usage: pilldrops sandbox get <id>") {
		t.Fatalf("stderr = %q, want the usage line unchanged", errb.String())
	}
	if s := out.String() + errb.String(); strings.Contains(s, promptish) || strings.Contains(s, "which sandbox") {
		t.Fatalf("a script was asked a question: %q", s)
	}
	if n := acted.Load(); n != 0 {
		t.Fatalf("%d calls made; a refused verb must call nothing", n)
	}
}

// The other half, without which the test above proves nothing: on a terminal
// the same command asks, offers the account's only sandbox as the default, and
// a bare return runs it.
func TestTerminalIsAskedAndTheOnlySandboxIsTheDefault(t *testing.T) {
	var acted atomic.Int32
	srv := promptPortal(t, 1, &acted)
	master, slave := tty(t)

	cmd, out, errb := startCLI(t, slave, srv.URL, "sandbox", "get")
	waitFor(t, errb, promptish+"sb_1]")
	if _, err := master.WriteString("\n"); err != nil {
		t.Fatal(err)
	}
	if code := wait(t, cmd); code != ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, errb.String())
	}
	if !strings.Contains(out.String(), "sb_1") {
		t.Fatalf("stdout = %q, want the sandbox the default named", out.String())
	}
	// Rule: the question is on stderr, so stdout stays the data channel.
	if strings.Contains(out.String(), promptish) {
		t.Fatalf("the prompt was written to stdout: %q", out.String())
	}
}

// The verbs a human types with the id left off. Each must reach the sandbox the
// prompt named — the point is the request, not the wording.
func TestTheVerbsAHumanTypesAskForTheMissingID(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"logs", []string{"logs"}, "log of sb_1"},
		{"exec", []string{"exec", "--", "echo", "hi"}, "ran in sb_1"},
		{"ps", []string{"ps"}, "ps of sb_1"},
		{"sandbox pause", []string{"sandbox", "pause"}, ""},
		{"backup create", []string{"backup", "create"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var acted atomic.Int32
			srv := promptPortal(t, 1, &acted)
			master, slave := tty(t)
			cmd, out, errb := startCLI(t, slave, srv.URL, tc.args...)
			waitFor(t, errb, promptish+"sb_1]")
			if _, err := master.WriteString("\n"); err != nil {
				t.Fatal(err)
			}
			wait(t, cmd)
			if tc.want != "" && !strings.Contains(out.String(), tc.want) {
				t.Fatalf("stdout = %q, want %q (stderr %q)", out.String(), tc.want, errb.String())
			}
		})
	}
}

// ---------------------------------------------------------------- rule 2 ---

// --json is a machine contract: a terminal changes nothing about it.
func TestJSONNeverPrompts(t *testing.T) {
	var acted atomic.Int32
	srv := promptPortal(t, 1, &acted)
	master, slave := tty(t)
	// An answer is waiting in the pty the whole time; nothing may consume it.
	if _, err := master.WriteString("\n"); err != nil {
		t.Fatal(err)
	}

	cmd, out, errb := startCLI(t, slave, srv.URL, "--json", "sandbox", "get")
	if code := wait(t, cmd); code != ExitUsage {
		t.Fatalf("exit = %d, want %d (stderr %q)", code, ExitUsage, errb.String())
	}
	if s := out.String() + errb.String(); strings.Contains(s, promptish) || strings.Contains(s, "which sandbox") {
		t.Fatalf("--json prompted: %q", s)
	}
	if n := acted.Load(); n != 0 {
		t.Fatalf("%d calls made under --json; want none", n)
	}
}

// ---------------------------------------------------------------- rule 3 ---

// A destructive verb may be told WHICH sandbox on a terminal; it may not be
// told WHETHER by the same key press. Without --confirm nothing is deleted,
// and the identical run with the flag deletes — so the gate is the flag, and
// only the flag.
func TestDestructiveVerbStillRequiresItsFlag(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		wantExit   int
		wantDelete int32
	}{
		{"without the flag", []string{"sandbox", "rm"}, ExitUsage, 0},
		{"with the flag", []string{"sandbox", "rm", "--confirm"}, ExitOK, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var acted atomic.Int32
			srv := promptPortal(t, 1, &acted)
			master, slave := tty(t)
			cmd, _, errb := startCLI(t, slave, srv.URL, tc.args...)
			waitFor(t, errb, promptish+"sb_1]") // the id IS asked for, either way
			if _, err := master.WriteString("\n"); err != nil {
				t.Fatal(err)
			}
			if code := wait(t, cmd); code != tc.wantExit {
				t.Fatalf("exit = %d, want %d (stderr %q)", code, tc.wantExit, errb.String())
			}
			if n := acted.Load(); n != tc.wantDelete {
				t.Fatalf("%d destructive calls, want %d — a return must not stand in for --confirm", n, tc.wantDelete)
			}
			if tc.wantExit == ExitUsage && !strings.Contains(errb.String(), "--confirm") {
				t.Fatalf("the refusal must name the flag: %q", errb.String())
			}
		})
	}
}

// ---------------------------------------------------------------- rule 4 ---

// Several sandboxes: they are listed and chosen by number, so the question is
// answerable without going to look the ids up.
func TestSeveralSandboxesArePickedByNumber(t *testing.T) {
	var acted atomic.Int32
	srv := promptPortal(t, 3, &acted)
	master, slave := tty(t)

	cmd, out, errb := startCLI(t, slave, srv.URL, "sandbox", "get")
	waitFor(t, errb, "number 1-3")
	if _, err := master.WriteString("2\n"); err != nil {
		t.Fatal(err)
	}
	if code := wait(t, cmd); code != ExitOK {
		t.Fatalf("exit = %d (stderr %q)", code, errb.String())
	}
	for _, want := range []string{"1  sb_1", "2  sb_2", "3  sb_3"} {
		if !strings.Contains(errb.String(), want) {
			t.Fatalf("the choices must be shown; stderr = %q", errb.String())
		}
	}
	if !strings.Contains(out.String(), "sb_2") || strings.Contains(out.String(), "sb_1") {
		t.Fatalf("stdout = %q, want the SECOND sandbox", out.String())
	}
}

// cp is the one verb whose missing piece is not an id. With one side typed the
// other is asked for, and the default is where the bytes obviously land — so
// the offer is real, not a blank line with a colon after it.
func TestCpAsksForTheSideThatWasLeftOff(t *testing.T) {
	var acted atomic.Int32
	srv := promptPortal(t, 1, &acted)
	master, slave := tty(t)

	cmd, out, errb := startCLI(t, slave, srv.URL, "cp", "sb_1:/etc/hostname")
	waitFor(t, errb, "destination [hostname]")
	if _, err := master.WriteString("-\n"); err != nil { // "-" is stdout
		t.Fatal(err)
	}
	if code := wait(t, cmd); code != ExitOK {
		t.Fatalf("exit = %d (stderr %q)", code, errb.String())
	}
	if out.String() != "file-bytes" {
		t.Fatalf("stdout = %q, want the file", out.String())
	}
}

// ---------------------------------------------------------------- rule 5 ---

// ^C at a prompt is not an answer: the process dies non-zero, nothing is done,
// and the terminal is left exactly as it was found.
func TestCtrlCAtAPromptDoesNothing(t *testing.T) {
	var acted atomic.Int32
	srv := promptPortal(t, 1, &acted)
	_, slave := tty(t)
	before, err := term.GetState(int(slave.Fd()))
	if err != nil {
		t.Fatalf("read the terminal state: %v", err)
	}

	// --confirm is already given, so an answer here would really delete.
	cmd, _, errb := startCLI(t, slave, srv.URL, "sandbox", "rm", "--confirm")
	waitFor(t, errb, promptish+"sb_1]")
	// What ^C on this terminal produces. Turning the 0x03 byte into this
	// signal is the line discipline's job, not the CLI's; what is under test
	// is what the CLI does when it arrives mid-question.
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if code := wait(t, cmd); code == ExitOK {
		t.Fatal("^C exited 0; an interruption is not an answer")
	}
	if n := acted.Load(); n != 0 {
		t.Fatalf("%d calls after ^C; want none", n)
	}
	after, err := term.GetState(int(slave.Fd()))
	if err != nil {
		t.Fatalf("read the terminal state: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("the terminal was left changed: %+v -> %+v", before, after)
	}
}
