package cli

import (
	"flag"
	"fmt"
	"net/url"
	"time"
)

// A snapshot is org-owned and outlives the sandbox it was taken from — it is
// what `sandbox create --from-snapshot` boots — so it is its own noun, not a
// `sandbox` subcommand. Only `save` names a sandbox at all.
const snapshotUsage = `usage: pilldrops snapshot save <id> <name> [--no-wait]
       pilldrops snapshot list
       pilldrops snapshot get <name>
       pilldrops snapshot rm <name> --confirm`

// A save writes the guest's whole RAM into the artifact store, so it is minutes
// on a large machine. Vars so a test does not sit through one.
var (
	snapshotPollFor   = 20 * time.Minute
	snapshotPollEvery = 3 * time.Second
)

// snapshotVersion is one version as /v1/snapshots reports it.
type snapshotVersion struct {
	Name      string `json:"name"`
	Version   int    `json:"version"`
	Status    string `json:"status"`
	BuildID   string `json:"build_id"`
	Runtime   string `json:"runtime"`
	Kind      string `json:"snapshot_kind"`
	Template  string `json:"template"`
	VCPU      int    `json:"vcpu"`
	MemMB     int    `json:"mem_mb"`
	Size      int64  `json:"size"`
	CreatedAt int64  `json:"created_at"`
}

func cmdSnapshot(c *cmdCtx, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(c.stderr, snapshotUsage)
		return ExitUsage
	}
	cl, err := c.client()
	if err != nil {
		return c.fail(err)
	}
	switch args[0] {
	case "save":
		return snapshotSave(c, cl, args[1:])

	case "list", "ls":
		var page struct {
			Snapshots []snapshotVersion `json:"snapshots"`
		}
		if err := cl.JSON("GET", "/v1/snapshots", nil, &page); err != nil {
			return c.fail(err)
		}
		if c.jsonOut {
			printJSON(c.stdout, page.Snapshots)
			return ExitOK
		}
		rows := [][]string{{"NAME", "VER", "KIND", "STATUS", "SIZE", "RUNTIME"}}
		for _, s := range page.Snapshots {
			rows = append(rows, []string{s.Name, fmt.Sprint(s.Version), s.Kind, s.Status, mib(s.Size), s.Runtime})
		}
		table(c.stdout, rows)
		return ExitOK

	case "get":
		if len(args) != 2 {
			fmt.Fprintln(c.stderr, snapshotUsage)
			return ExitUsage
		}
		var out map[string]any
		if err := cl.JSON("GET", "/v1/snapshots/"+url.PathEscape(args[1]), nil, &out); err != nil {
			return c.fail(err)
		}
		if c.jsonOut {
			printJSON(c.stdout, out)
			return ExitOK
		}
		rows := [][]string{}
		for _, k := range []string{"name", "version", "status", "snapshot_kind", "runtime",
			"template", "vcpu", "mem_mb", "size", "build_id"} {
			v, ok := out[k]
			if !ok {
				continue
			}
			// A snapshot is gigabytes, and JSON numbers arrive as float64:
			// printed raw, the size reads as 1.837228572e+09.
			if k == "size" {
				v = mib(int64(toFloat(v)))
			}
			rows = append(rows, []string{k, str(v)})
		}
		table(c.stdout, rows)
		return ExitOK

	case "rm", "delete":
		// Every version goes, and the objects with them. Nothing restores it,
		// so the intent is typed rather than assumed.
		rest, confirm := takeConfirm(args[1:])
		if len(rest) != 1 {
			fmt.Fprintln(c.stderr, snapshotUsage)
			return ExitUsage
		}
		name := rest[0]
		if !confirm {
			fmt.Fprintf(c.stderr, "pilldrops snapshot rm: %s would lose every version — re-run with --confirm\n", name)
			return ExitUsage
		}
		var out map[string]any
		if err := cl.JSON("DELETE", "/v1/snapshots/"+url.PathEscape(name), nil, &out); err != nil {
			return c.fail(err)
		}
		if c.jsonOut {
			printJSON(c.stdout, out)
		} else if !c.quiet {
			fmt.Fprintf(c.stdout, "deleted %s (%s version(s))\n", name, str(out["deleted_versions"]))
		}
		return ExitOK
	}
	fmt.Fprintln(c.stderr, snapshotUsage)
	return ExitUsage
}

func snapshotSave(c *cmdCtx, cl *Client, args []string) int {
	fs := flag.NewFlagSet("save", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	noWait := fs.Bool("no-wait", false, "print the building version and return, instead of waiting for it to be ready")
	if len(args) < 2 {
		fmt.Fprintln(c.stderr, snapshotUsage)
		return ExitUsage
	}
	if err := fs.Parse(args[2:]); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(c.stderr, snapshotUsage)
		return ExitUsage
	}
	id, name := args[0], args[1]
	var started snapshotVersion
	if err := cl.JSON("POST", "/v1/sandboxes/"+id+"/snapshot", map[string]any{"name": name}, &started); err != nil {
		return c.fail(err)
	}
	if *noWait {
		if c.jsonOut {
			printJSON(c.stdout, started)
		} else if !c.quiet {
			fmt.Fprintf(c.stdout, "%s v%d %s (%s) — poll with `pilldrops snapshot get %s`\n",
				started.Name, started.Version, started.Status, started.Kind, started.Name)
		}
		return ExitOK
	}
	if !c.quiet && !c.jsonOut {
		// The save is asynchronous and the memfile is the guest's whole RAM;
		// silence here reads as a hang.
		fmt.Fprintf(c.stdout, "saving %s v%d from %s (%s) — this takes minutes on a large machine\n",
			started.Name, started.Version, id, started.Kind)
	}
	done, err := awaitSnapshot(cl, name, started.BuildID)
	if err != nil {
		return c.fail(err)
	}
	if c.jsonOut {
		printJSON(c.stdout, done)
		return ExitOK
	}
	if done.Status != "ready" {
		fmt.Fprintf(c.stderr, "snapshot %s v%d ended %s\n", done.Name, done.Version, done.Status)
		return ExitAPI
	}
	if !c.quiet {
		fmt.Fprintf(c.stdout, "%s v%d ready — %s, %d vCPU / %d MB\n",
			done.Name, done.Version, mib(done.Size), done.VCPU, done.MemMB)
	}
	return ExitOK
}

// awaitSnapshot polls the name until the version this save started leaves
// `building`. Matched on build id, not on the name: another save of the same
// name would otherwise report the wrong version's outcome.
func awaitSnapshot(cl *Client, name, buildID string) (snapshotVersion, error) {
	deadline := time.Now().Add(snapshotPollFor)
	for {
		var out struct {
			Versions []snapshotVersion `json:"versions"`
		}
		if err := cl.JSON("GET", "/v1/snapshots/"+url.PathEscape(name), nil, &out); err != nil {
			return snapshotVersion{}, err
		}
		for _, v := range out.Versions {
			if v.BuildID == buildID && v.Status != "building" {
				return v, nil
			}
		}
		if time.Now().After(deadline) {
			return snapshotVersion{}, fmt.Errorf("timed out after %s waiting for snapshot %s to be ready (it may still finish — `pilldrops snapshot get %s`)",
				snapshotPollFor, name, name)
		}
		time.Sleep(snapshotPollEvery)
	}
}

// takeConfirm splits --confirm (or -y) out of an argument list, wherever the
// user put it. Every destructive verb shares it so one spelling works on all of
// them and the flag is never mistaken for the thing being deleted.
//
// The flag is always required and is never itself a question, TTY or not: a
// prompt a returned key press satisfies turns a deliberate gate into a reflex,
// and half of these verbs are typed by agents that have no terminal at all.
// One rule, one exit code (2), one thing to script around.
//
// WHICH thing to destroy is a different question, and on a terminal it is
// asked (prompt.go): being handed the id you meant does not decide anything,
// and the flag still has to be there afterwards.
func takeConfirm(args []string) (rest []string, confirm bool) {
	for _, a := range args {
		switch a {
		case "--confirm", "-confirm", "-y", "--yes":
			confirm = true
		default:
			rest = append(rest, a)
		}
	}
	return rest, confirm
}

// mib renders a byte count for a table cell; the exact number is in --json.
func mib(b int64) string {
	if b <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
}
