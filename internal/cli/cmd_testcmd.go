package cli

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"
)

// testPollInterval is how often `pilldrops test` polls the session status while
// waiting for a run to finish. A var so tests can shrink it.
var testPollInterval = 5 * time.Second

// cmdTest is the one-command mobile E2E flow: create a mobile-test sandbox
// (or reuse one), upload the script to /test/run.sh, and watch the injected
// session harness run it — live progress here, live page + final report
// (video, screenshot, artifacts) at the sandbox's preview URL.
func cmdTest(c *cmdCtx, args []string) int {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	tmpl := fs.String("template", "mobile-test", "session template to create")
	sandbox := fs.String("sandbox", "", "reuse an existing session sandbox instead of creating one")
	timeout := fs.Int("timeout", 5400, "sandbox lifetime seconds (new sandboxes)")
	noWait := fs.Bool("no-wait", false, "upload and print the session URL without waiting for the verdict")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	// Go's flag package stops at the first positional arg; users naturally type
	// `pilldrops test suite.sh --no-wait`, so re-parse anything after the script.
	rest := fs.Args()
	if len(rest) > 1 {
		if err := fs.Parse(rest[1:]); err != nil {
			return ExitUsage
		}
		if fs.NArg() != 0 {
			rest = nil // stray extra positionals → usage
		}
	}
	if len(rest) < 1 {
		fmt.Fprintln(c.stderr, "usage: pilldrops test <script.sh> [--template T] [--sandbox ID] [--timeout S] [--no-wait]")
		return ExitUsage
	}
	script, err := os.ReadFile(rest[0])
	if err != nil {
		fmt.Fprintf(c.stderr, "pilldrops test: %v\n", err)
		return ExitUsage
	}
	cl, err := c.client()
	if err != nil {
		return c.fail(err)
	}

	id, url := *sandbox, ""
	if id == "" {
		// idle_timeout -1: pausing kills the emulator (and the report server) —
		// session sandboxes must stay hot for their whole lifetime.
		req := map[string]any{"template": *tmpl, "timeout": *timeout, "idle_timeout": -1}
		var out map[string]any
		if err := cl.JSON("POST", "/v1/sandboxes", req, &out); err != nil {
			return c.fail(err)
		}
		id, url = str(out["id"]), str(out["url"])
		if !c.quiet {
			fmt.Fprintf(c.stdout, "sandbox: %s\n", id)
		}
	}
	if url != "" && !c.quiet {
		fmt.Fprintf(c.stdout, "session: %s  (open it — live progress now, full report when done)\n", url)
	}

	if err := uploadRunScript(cl, id, script); err != nil {
		return c.fail(err)
	}
	if !c.quiet {
		fmt.Fprintln(c.stdout, "test uploaded → /test/run.sh — the harness runs it automatically")
	}
	if *noWait {
		return ExitOK
	}

	// Poll the harness status until the run completes. Ceiling = the sandbox
	// lifetime: after that the sandbox is gone and the verdict unreachable.
	verdict, status, err := waitForVerdict(cl, id, time.Duration(*timeout)*time.Second, c)
	if err != nil {
		return c.fail(err)
	}
	if c.jsonOut {
		printJSON(c.stdout, status)
	} else {
		fmt.Fprintf(c.stdout, "verdict: %s\n", verdict)
		if url != "" {
			fmt.Fprintf(c.stdout, "report:  %s\n", url)
		}
	}
	if verdict != "GREEN" {
		return ExitAPI
	}
	return ExitOK
}

// uploadRunScript ships the script through exec in base64 chunks (the files API
// is not available on every node path), writing a temp file first and mv-ing it
// into place so the harness's change-watcher never sees a half-written script.
func uploadRunScript(cl *Client, id string, script []byte) error {
	exec := func(cmd string) error {
		var out struct {
			ExitCode int    `json:"exit_code"`
			Stderr   string `json:"stderr"`
		}
		if err := cl.JSON("POST", "/v1/sandboxes/"+id+"/exec", map[string]any{"cmd": cmd}, &out); err != nil {
			return err
		}
		if out.ExitCode != 0 {
			return fmt.Errorf("upload step failed (exit %d): %s", out.ExitCode, out.Stderr)
		}
		return nil
	}
	if err := exec("rm -f /test/.upload.b64 /test/.upload.sh"); err != nil {
		return err
	}
	b64 := base64.StdEncoding.EncodeToString(script)
	const chunk = 48 * 1024
	for i := 0; i < len(b64); i += chunk {
		end := i + chunk
		if end > len(b64) {
			end = len(b64)
		}
		if err := exec(fmt.Sprintf("printf %%s %s >> /test/.upload.b64", b64[i:end])); err != nil {
			return err
		}
	}
	return exec("base64 -d /test/.upload.b64 > /test/.upload.sh && mv /test/.upload.sh /test/run.sh && rm -f /test/.upload.b64")
}

// waitForVerdict polls the harness status.json inside the sandbox, echoing step
// transitions, until state=done. Returns the verdict and the final status doc.
func waitForVerdict(cl *Client, id string, ceiling time.Duration, c *cmdCtx) (string, map[string]any, error) {
	type step struct {
		N string `json:"n"`
		S string `json:"s"`
	}
	var doc struct {
		State   string `json:"state"`
		Verdict string `json:"verdict"`
		Steps   []step `json:"steps"`
	}
	seen := map[string]string{}
	deadline := time.Now().Add(ceiling)
	for time.Now().Before(deadline) {
		var out struct {
			Stdout   string `json:"stdout"`
			ExitCode int    `json:"exit_code"`
		}
		err := cl.JSON("POST", "/v1/sandboxes/"+id+"/exec",
			map[string]any{"cmd": "cat /tmp/pd-session/status.json 2>/dev/null"}, &out)
		if err == nil && out.ExitCode == 0 && json.Unmarshal([]byte(out.Stdout), &doc) == nil {
			for _, s := range doc.Steps {
				if seen[s.N] != s.S && (s.S == "run" || s.S == "pass" || s.S == "fail") {
					seen[s.N] = s.S
					if !c.quiet && !c.jsonOut {
						icon := map[string]string{"run": "◐", "pass": "✓", "fail": "✗"}[s.S]
						fmt.Fprintf(c.stdout, "  %s %s\n", icon, s.N)
					}
				}
			}
			if doc.State == "done" {
				var full map[string]any
				_ = json.Unmarshal([]byte(out.Stdout), &full)
				return doc.Verdict, full, nil
			}
		}
		time.Sleep(testPollInterval)
	}
	return "", nil, fmt.Errorf("timed out after %s waiting for the run to finish (session may still complete — check the sandbox URL)", ceiling)
}
