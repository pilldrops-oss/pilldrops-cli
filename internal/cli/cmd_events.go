package cli

import (
	"fmt"
	"time"
)

// cmdEvents lists recent lifecycle events for the org, newest first — created,
// destroyed, paused, resumed, and `oom` (a process was out-of-memory-killed
// inside the sandbox). With an id it shows just that sandbox's events, which is
// how an agent notices a runaway process got reaped after a pause/resume or a
// long run.
//
//	pilldrops events            recent events across all sandboxes
//	pilldrops events <id>       just this sandbox's events
//	pilldrops --json events     raw records
func cmdEvents(c *cmdCtx, args []string) int {
	var filterID string
	switch len(args) {
	case 0:
	case 1:
		filterID = args[0]
	default:
		fmt.Fprintln(c.stderr, "usage: pilldrops events [<id>]")
		return ExitUsage
	}
	cl, err := c.client()
	if err != nil {
		return c.fail(err)
	}
	var resp struct {
		Events []map[string]any `json:"events"`
	}
	if err := cl.JSON("GET", "/v1/events?limit=200", nil, &resp); err != nil {
		return c.fail(err)
	}

	// The filter is applied here, not by the server — /v1/events takes no id —
	// and an event record carries only the canonical sandbox_id. So a slug,
	// which the help says works wherever an id does and which `get`, `metrics`,
	// `ps` and `checkpoint list` all accept, matched nothing: an empty table and
	// exit 0, which reads as "this sandbox did nothing".
	filterID = canonicalSandboxID(cl, filterID)

	rows := [][]string{{"TIME", "SANDBOX", "EVENT", "TEMPLATE"}}
	filtered := make([]map[string]any, 0, len(resp.Events))
	for _, e := range resp.Events {
		if filterID != "" && str(e["sandbox_id"]) != filterID {
			continue
		}
		filtered = append(filtered, e)
		rows = append(rows, []string{
			eventTime(e["at"]), str(e["sandbox_id"]), str(e["event"]), str(e["template"]),
		})
	}

	if c.jsonOut {
		printJSON(c.stdout, filtered)
		return ExitOK
	}
	table(c.stdout, rows)
	if filterID != "" && len(filtered) == 0 {
		// Nothing failed, so still exit 0 — but a bare header row is the same
		// output as a command that did not work.
		fmt.Fprintf(c.stderr, "no events for %s in the last 200 across this org\n", args[0])
	}
	return ExitOK
}

// canonicalSandboxID resolves whatever the caller typed to the id the records
// carry. A slug resolves through the routes that already accept one — the live
// sandbox first, then its history record, which is where a destroyed sandbox's
// slug survives. Best effort: anything unresolvable is returned unchanged, so an
// id (the common case, and one round trip cheaper) is unaffected either way.
func canonicalSandboxID(cl *Client, nameOrID string) string {
	if nameOrID == "" {
		return ""
	}
	for _, route := range []string{"/v1/sandboxes/", "/v1/history/"} {
		var out map[string]any
		if err := cl.JSON("GET", route+nameOrID, nil, &out); err == nil {
			if id := str(out["id"]); id != "" {
				return id
			}
		}
	}
	return nameOrID
}

// eventTime renders a unix-seconds timestamp (a JSON number → float64) as a local
// clock time, or "" when absent.
func eventTime(v any) string {
	n := int64(numOf(v))
	if n == 0 {
		return ""
	}
	return time.Unix(n, 0).Format("2006-01-02 15:04:05")
}
