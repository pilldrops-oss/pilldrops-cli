//go:build darwin || linux

package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/term"
)

// shellOnTTY runs bare `pilldrops` as a person would: the pty is all three
// streams and the controlling terminal, so a 0x03 byte is a real SIGINT from the
// line discipline and every child process the shell starts is really started.
type shellOnTTY struct {
	t      *testing.T
	cmd    *exec.Cmd
	master *os.File
	slave  *os.File
	screen *syncBuf
}

func startShell(t *testing.T, apiURL, key string, args ...string) *shellOnTTY {
	t.Helper()
	master, slave := tty(t)
	screen := &syncBuf{}
	go func() { _, _ = io.Copy(screen, master) }()
	cmd := exec.Command(os.Args[0], append([]string{"-test.run=TestPromptChild", "--"}, args...)...)
	cmd.Env = append(os.Environ(), "PILLDROPS_PROMPT_CHILD=1",
		"SANDFORGE_CONFIG="+filepath.Join(t.TempDir(), "config.json"),
		"PILLDROPS_API_URL="+apiURL, "PILLDROPS_API_KEY="+key,
		"SANDFORGE_API_URL=", "SANDFORGE_API_KEY=", "SANDFORGE_LOGS_POLL_MS=10")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return &shellOnTTY{t, cmd, master, slave, screen}
}

func (s *shellOnTTY) send(text string) {
	s.t.Helper()
	if _, err := s.master.WriteString(text); err != nil {
		s.t.Fatal(err)
	}
}

// waitPrompts blocks until the shell has printed its nth prompt.
func (s *shellOnTTY) waitPrompts(n int) {
	s.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for strings.Count(s.screen.String(), shellPrompt) < n {
		if time.Now().After(deadline) {
			s.t.Fatalf("never saw prompt %d; screen %q", n, s.screen.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *shellOnTTY) exit() {
	s.t.Helper()
	s.send("exit\n")
	if code := wait(s.t, s.cmd); code != ExitOK {
		s.t.Fatalf("exit status %d; screen %q", code, s.screen.String())
	}
}

func TestShellSurvivesCtrlCAndFailingCommands(t *testing.T) {
	var acted atomic.Int32
	srv := promptPortal(t, 1, &acted)
	s := startShell(t, srv.URL, "sfc_good")
	s.waitPrompts(1)

	s.send("partial line\x03") // Ctrl-C at the prompt abandons the line
	s.waitPrompts(2)
	s.send("sandbox frobnicate\n") // exits 2 in its child
	s.waitPrompts(3)
	if !strings.Contains(s.screen.String(), `unknown subcommand "frobnicate"`) {
		t.Fatalf("the failure was not shown: %q", s.screen.String())
	}
	s.send("sandbox ls\n")
	s.waitPrompts(4)
	if !strings.Contains(s.screen.String(), "sb_1") {
		t.Fatalf("screen %q", s.screen.String())
	}
	s.exit()
}

// logs -f only ever ends by being killed, and that must not take the shell along.
func TestCtrlCStopsTheRunningCommandAndNotTheShell(t *testing.T) {
	var acted atomic.Int32
	srv := promptPortal(t, 1, &acted)
	s := startShell(t, srv.URL, "sfc_good")
	s.waitPrompts(1)
	s.send("logs sb_1 -f\n")
	waitFor(t, s.screen, "log of sb_1")
	s.send("\x03")
	s.waitPrompts(2)
	s.exit()
}

func TestCtrlDLeavesTheShell(t *testing.T) {
	var acted atomic.Int32
	srv := promptPortal(t, 1, &acted)
	s := startShell(t, srv.URL, "sfc_good")
	s.waitPrompts(1)
	s.send("\x04")
	if code := wait(t, s.cmd); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
}

// connect takes the terminal raw and leaves a goroutine blocked reading stdin
// forever. Run in-process, that goroutine would eat the next line typed here.
func TestConnectReturnsToTheShellWithTheTerminalIntact(t *testing.T) {
	srv := fakeTerminal(t, func(ws *websocket.Conn) {
		_ = ws.WriteMessage(websocket.BinaryMessage, []byte("remote$ "))
		for {
			mt, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if mt == websocket.BinaryMessage && strings.ContainsRune(string(data), 0x04) {
				_ = ws.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
				return
			}
		}
	})
	s := startShell(t, srv.URL, "sfc_good")
	s.waitPrompts(1)
	before, err := term.GetState(int(s.slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	s.send("connect sb_1\n")
	waitFor(t, s.screen, "remote$ ")
	s.send("\x04") // the remote shell ends
	s.waitPrompts(2)
	after, err := term.GetState(int(s.slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("the terminal came back changed: %+v -> %+v", before, after)
	}
	s.send("version\n")
	waitFor(t, s.screen, Version)
	s.waitPrompts(3)
	s.exit()
}

func TestShellLogsInFirstWhenLoggedOut(t *testing.T) {
	srv := fakePlatform(t, `{"org":"acme","principal":"key"}`)
	defer srv.Close()
	s := startShell(t, srv.URL, "")
	waitFor(t, s.screen, "Server URL ["+srv.URL+"]: ")
	s.send("\n")
	waitFor(t, s.screen, "API key: ")
	time.Sleep(200 * time.Millisecond) // the prompt is printed just before echo goes off
	s.send("sf_good\n")
	s.waitPrompts(1)
	s.send("whoami\n") // a child process, reading the login just stored
	waitFor(t, s.screen, "acme")
	s.waitPrompts(2)
	s.exit()
	if strings.Contains(s.screen.String(), "sf_good") {
		t.Fatalf("the key reached the screen: %q", s.screen.String())
	}
}

func TestRejectedLoginLeavesTheShellClosed(t *testing.T) {
	srv := fakePlatform(t, `{}`)
	defer srv.Close()
	s := startShell(t, srv.URL, "")
	waitFor(t, s.screen, "Server URL [")
	s.send("\n")
	waitFor(t, s.screen, "API key: ")
	time.Sleep(200 * time.Millisecond)
	s.send("sf_wrong\n")
	if code := wait(t, s.cmd); code != ExitAuth {
		t.Fatalf("exit %d, want %d", code, ExitAuth)
	}
}

// Bare pilldrops opens the shell only with a terminal on both streams and no --json.
func TestBareInvocationStaysUsageForAnythingAutomated(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	s := startShell(t, srv.URL, "sfc_good", "--json")
	if code := wait(t, s.cmd); code != ExitUsage {
		t.Fatalf("--json on a terminal = %d", code)
	}

	_, slave := tty(t)
	cmd, _, errb := startCLI(t, slave, srv.URL) // stdin a terminal, stdout a pipe
	if code := wait(t, cmd); code != ExitUsage || !strings.Contains(errb.String(), "usage:") {
		t.Fatalf("piped stdout = %d, stderr %q", code, errb.String())
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()
	cmd, _, _ = startCLI(t, r, srv.URL) // stdin a pipe
	if code := wait(t, cmd); code != ExitUsage {
		t.Fatalf("piped stdin = %d", code)
	}
}
