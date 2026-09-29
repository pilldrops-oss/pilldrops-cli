package cli

import (
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// A checkpoint is a named save point that lives on the sandbox's own node and
// dies with it — unlike a snapshot, which is org-owned and outlives it. Its own
// noun for the same reason `domain` and `env` are: four verbs of its own.
const checkpointUsage = `usage: pilldrops checkpoint save <id> <name>
       pilldrops checkpoint list <id>
       pilldrops checkpoint rm <id> <name> --confirm
       pilldrops checkpoint restore <id> <name> --confirm`

// checkpoint mirrors the /v1/sandboxes/{id}/checkpoints wire shape.
type checkpoint struct {
	Name      string `json:"name"`
	Kind      string `json:"snapshot_kind"`
	Status    string `json:"status"`
	SizeBytes int64  `json:"size_bytes"`
	PausedMs  int64  `json:"paused_ms"`
	// RFC3339, unlike the unix seconds a backup or a snapshot reports.
	CreatedAt time.Time `json:"created_at"`
}

func cmdCheckpoint(c *cmdCtx, args []string) int {
	// Taken out before the positionals are read, so --confirm works wherever
	// the user put it rather than being mistaken for the sandbox id.
	args, confirmed := takeConfirm(args)
	// `checkpoint list` is the only form whose one missing piece is the id;
	// save, rm and restore also want a name, which nothing here can propose.
	if len(args) == 1 && (args[0] == "list" || args[0] == "ls") {
		args = append(args, c.wantID("checkpoint "+args[0], nil)...)
	}
	if len(args) < 2 {
		fmt.Fprintln(c.stderr, checkpointUsage)
		return ExitUsage
	}
	cl, err := c.client()
	if err != nil {
		return c.fail(err)
	}
	sub, id, rest := args[0], args[1], args[2:]
	base := "/v1/sandboxes/" + id + "/checkpoints"

	switch sub {
	case "list", "ls":
		if len(rest) != 0 {
			fmt.Fprintln(c.stderr, checkpointUsage)
			return ExitUsage
		}
		var page struct {
			Checkpoints []checkpoint `json:"checkpoints"`
			Max         int          `json:"max"`
		}
		if err := cl.JSON("GET", base, nil, &page); err != nil {
			return c.fail(err)
		}
		if c.jsonOut {
			printJSON(c.stdout, page.Checkpoints)
			return ExitOK
		}
		rows := [][]string{{"NAME", "KIND", "STATUS", "SIZE", "PAUSED", "CREATED"}}
		for _, cp := range page.Checkpoints {
			rows = append(rows, []string{cp.Name, cp.Kind, cp.Status, mib(cp.SizeBytes),
				strconv.FormatInt(cp.PausedMs, 10) + "ms", stamp(cp.CreatedAt.Unix())})
		}
		table(c.stdout, rows)
		if !c.quiet {
			fmt.Fprintf(c.stdout, "%d of %d\n", len(page.Checkpoints), page.Max)
		}
		return ExitOK

	case "save":
		if len(rest) != 1 {
			fmt.Fprintln(c.stderr, checkpointUsage)
			return ExitUsage
		}
		var cp checkpoint
		if err := cl.JSON("POST", base, map[string]any{"name": rest[0]}, &cp); err != nil {
			return c.fail(err)
		}
		if c.jsonOut {
			printJSON(c.stdout, cp)
		} else if !c.quiet {
			fmt.Fprintf(c.stdout, "saved %s on %s — %s, %s, guest paused %dms\n",
				cp.Name, id, cp.Kind, mib(cp.SizeBytes), cp.PausedMs)
		}
		return ExitOK

	case "rm", "delete":
		// A save point is gone for good, and the machine it was taken on keeps
		// running — same rule as every other destructive verb here.
		if len(rest) != 1 {
			fmt.Fprintln(c.stderr, checkpointUsage)
			return ExitUsage
		}
		if !confirmed {
			fmt.Fprintf(c.stderr, "pilldrops checkpoint rm: %s on %s is deleted and nothing restores it — re-run with --confirm\n", rest[0], id)
			return ExitUsage
		}
		var out map[string]any
		if err := cl.JSON("DELETE", base+"/"+url.PathEscape(rest[0]), nil, &out); err != nil {
			return c.fail(err)
		}
		if c.jsonOut {
			printJSON(c.stdout, out)
		} else if !c.quiet {
			fmt.Fprintf(c.stdout, "removed %s from %s\n", rest[0], id)
		}
		return ExitOK

	case "restore":
		// Rolling back throws away every write made since the save point, on a
		// machine that is running right now. Typed, not assumed.
		if len(rest) != 1 {
			fmt.Fprintln(c.stderr, checkpointUsage)
			return ExitUsage
		}
		name := rest[0]
		if !confirmed {
			fmt.Fprintf(c.stderr, "pilldrops checkpoint restore: %s loses every write made since %s — re-run with --confirm\n", id, name)
			return ExitUsage
		}
		var out map[string]any
		if err := cl.JSON("POST", base+"/"+url.PathEscape(name)+"/restore", nil, &out); err != nil {
			return c.fail(err)
		}
		if c.jsonOut {
			printJSON(c.stdout, out)
		} else if !c.quiet {
			fmt.Fprintf(c.stdout, "%s restored to %s in %sms\n", id, name, str(out["restore_ms"]))
		}
		return ExitOK
	}
	fmt.Fprintln(c.stderr, checkpointUsage)
	return ExitUsage
}
