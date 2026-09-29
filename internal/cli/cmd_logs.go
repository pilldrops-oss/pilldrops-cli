package cli

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// cmdLogs tails the sandbox app log; -f poll-follows the /v1 tail endpoint
// (the SSE stream is node-local and not proxied through the portal).
func cmdLogs(c *cmdCtx, args []string) int {
	follow := false
	id := ""
	for _, a := range args {
		if a == "-f" || a == "--follow" {
			follow = true
			continue
		}
		if id == "" {
			id = a
			continue
		}
		fmt.Fprintln(c.stderr, "usage: pilldrops logs <id> [-f]")
		return ExitUsage
	}
	if id == "" {
		if picked, ok := c.askSandboxID("logs"); ok {
			id = picked
		}
	}
	if id == "" {
		fmt.Fprintln(c.stderr, "usage: pilldrops logs <id> [-f]")
		return ExitUsage
	}
	cl, err := c.client()
	if err != nil {
		return c.fail(err)
	}

	lastNote := ""
	fetch := func() ([]string, error) {
		var out struct {
			Lines []string `json:"lines"`
			Note  string   `json:"note"`
		}
		err := cl.JSON("GET", "/v1/sandboxes/"+id+"/logs?limit=500", nil, &out)
		lastNote = out.Note
		return out.Lines, err
	}

	lines, err := fetch()
	if err != nil {
		return c.fail(err)
	}
	if c.jsonOut && !follow {
		printJSON(c.stdout, lines)
		return ExitOK
	}
	for _, l := range lines {
		fmt.Fprintln(c.stdout, l)
	}
	// When there's nothing to show, explain why on stderr (keeps stdout clean for
	// piping) so an agent isn't left staring at empty output.
	if len(lines) == 0 && lastNote != "" && !c.quiet {
		fmt.Fprintln(c.stderr, "pilldrops: "+lastNote)
	}
	if !follow {
		return ExitOK
	}

	// Poll-follow: print only lines beyond what we've already shown. If the
	// server tail shrinks (rotation), reprint from the top of the new tail.
	interval := 2 * time.Second
	if v := os.Getenv("SANDFORGE_LOGS_POLL_MS"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms > 0 {
			interval = time.Duration(ms) * time.Millisecond
		}
	}
	maxPolls := -1 // -1 = forever; test hook bounds it
	if v := os.Getenv("SANDFORGE_LOGS_MAX_POLLS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			maxPolls = n
		}
	}
	seen := len(lines)
	polls := 1
	failures := 0
	for maxPolls < 0 || polls < maxPolls {
		time.Sleep(interval)
		polls++
		lines, err := fetch()
		if err != nil {
			if failures++; failures >= 3 {
				return c.fail(err)
			}
			continue
		}
		failures = 0
		if len(lines) < seen {
			seen = 0 // tail rotated/shrank — start over
		}
		for _, l := range lines[seen:] {
			fmt.Fprintln(c.stdout, l)
		}
		seen = len(lines)
	}
	return ExitOK
}
