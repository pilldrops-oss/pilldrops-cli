//go:build darwin || linux

package cli

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/term"
)

// startLogin runs the real CLI on a pty, as prompt_pty_test does, with a config
// path the test can read back.
func startLogin(t *testing.T, slave *os.File, cfg, apiURL string) (*exec.Cmd, *syncBuf, *syncBuf) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestPromptChild", "--", "login", "--api-url", apiURL)
	cmd.Env = append(os.Environ(), "PILLDROPS_PROMPT_CHILD=1", "SANDFORGE_CONFIG="+cfg,
		"PILLDROPS_API_URL=", "PILLDROPS_API_KEY=", "SANDFORGE_API_URL=", "SANDFORGE_API_KEY=")
	out, errb := &syncBuf{}, &syncBuf{}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, out, errb
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd, out, errb
}

// answerServer takes the default server and leaves the child waiting on the key.
func answerServer(t *testing.T, master *os.File, errb *syncBuf, apiURL string) {
	t.Helper()
	waitFor(t, errb, "Server URL ["+apiURL+"]: ")
	if _, err := master.WriteString("\n"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, errb, apiURL+"/dashboard")
	waitFor(t, errb, "API key: ")
	time.Sleep(200 * time.Millisecond) // the prompt is printed just before echo goes off
}

func TestInteractiveLoginStoresAVerifiedKeyWithoutEchoingIt(t *testing.T) {
	srv := fakePlatform(t, `{"org":"acme","principal":"key"}`)
	defer srv.Close()
	master, slave := tty(t)
	screen := &syncBuf{}
	go func() { _, _ = io.Copy(screen, master) }()
	cfg := filepath.Join(t.TempDir(), "config.json")

	cmd, out, errb := startLogin(t, slave, cfg, srv.URL)
	answerServer(t, master, errb, srv.URL)
	if _, err := master.WriteString("sf_good\n"); err != nil {
		t.Fatal(err)
	}
	if code := wait(t, cmd); code != ExitOK {
		t.Fatalf("exit = %d, stderr %q", code, errb.String())
	}
	t.Setenv("SANDFORGE_CONFIG", cfg)
	if got, _ := LoadConfig(); got.Key != "sf_good" || got.APIURL != srv.URL {
		t.Fatalf("stored %+v", got)
	}
	time.Sleep(100 * time.Millisecond) // let the copier drain the pty
	if s := out.String() + errb.String() + screen.String(); strings.Contains(s, "sf_good") {
		t.Fatalf("the key was printed or echoed: %q", s)
	}
}

func TestInteractiveLoginRejectionStoresNothing(t *testing.T) {
	srv := fakePlatform(t, `{"org":"acme","principal":"key"}`)
	defer srv.Close()
	master, slave := tty(t)
	cfg := filepath.Join(t.TempDir(), "config.json")

	cmd, _, errb := startLogin(t, slave, cfg, srv.URL)
	answerServer(t, master, errb, srv.URL)
	if _, err := master.WriteString("sf_wrong\n"); err != nil {
		t.Fatal(err)
	}
	if code := wait(t, cmd); code != ExitAuth {
		t.Fatalf("exit = %d, want %d, stderr %q", code, ExitAuth, errb.String())
	}
	if !strings.Contains(errb.String(), "nothing was stored") || strings.Contains(errb.String(), "sf_wrong") {
		t.Fatalf("stderr = %q", errb.String())
	}
	if _, err := os.Stat(cfg); !os.IsNotExist(err) {
		t.Fatalf("config exists after a rejected key: %v", err)
	}
}

// Echo is off while the key is read, so a ^C that did not restore it would
// leave the user typing blind in their own shell.
func TestCtrlCAtTheKeyPromptRestoresTheTerminal(t *testing.T) {
	srv := fakePlatform(t, `{}`)
	defer srv.Close()
	master, slave := tty(t)
	before, err := term.GetState(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(t.TempDir(), "config.json")

	cmd, _, errb := startLogin(t, slave, cfg, srv.URL)
	answerServer(t, master, errb, srv.URL)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if code := wait(t, cmd); code != ExitUsage {
		t.Fatalf("exit = %d, want %d", code, ExitUsage)
	}
	after, err := term.GetState(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("the terminal was left changed: %+v -> %+v", before, after)
	}
	if _, err := os.Stat(cfg); !os.IsNotExist(err) {
		t.Fatalf("config exists after ^C: %v", err)
	}
}

func TestInteractiveLoginEmptyKeyIsUsage(t *testing.T) {
	srv := fakePlatform(t, `{}`)
	defer srv.Close()
	master, slave := tty(t)
	cmd, _, errb := startLogin(t, slave, filepath.Join(t.TempDir(), "config.json"), srv.URL)
	answerServer(t, master, errb, srv.URL)
	if _, err := master.WriteString("\n"); err != nil {
		t.Fatal(err)
	}
	if code := wait(t, cmd); code != ExitUsage {
		t.Fatalf("exit = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(errb.String(), loginUsage) {
		t.Fatalf("stderr = %q", errb.String())
	}
}
