package cli

import (
	"flag"
	"fmt"
	"net/url"
	"time"
)

// A backup is a capture of one sandbox pushed to the deployment's object store,
// on demand or on a schedule. Its own noun: five verbs, one of which (schedule)
// is not a sandbox action at all.
const backupUsage = `usage: pilldrops backup create <id> [--no-wait]
       pilldrops backup list <id>
       pilldrops backup rm <id> <backup> --confirm
       pilldrops backup restore <id> <backup> --confirm
       pilldrops backup schedule <id> [--every 24h | --cron '0 3 * * *' | --off] [--retain 7d]`

// A capture pauses the guest, writes a disk image and uploads it, so the wait
// is minutes. Vars so a test does not sit through one.
var (
	backupPollFor   = 30 * time.Minute
	backupPollEvery = 5 * time.Second
)

// backup mirrors the /v1/sandboxes/{id}/backups wire shape.
type backup struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Bytes     int64  `json:"bytes"`
	Kind      string `json:"snapshot_kind"`
	CreatedAt int64  `json:"created_at"`
	ReadyAt   int64  `json:"ready_at"`
	ExpiresAt int64  `json:"expires_at"`
	ParentID  string `json:"parent_id"`
	Deduped   bool   `json:"deduped"`
	PausedMs  int64  `json:"paused_ms"`
	Error     string `json:"error"`
}

func cmdBackup(c *cmdCtx, args []string) int {
	// Taken out before the positionals are read, so --confirm works wherever
	// the user put it rather than being mistaken for the sandbox id.
	args, confirmed := takeConfirm(args)
	// create and list take nothing but the id; rm and restore also want a
	// backup id, and schedule a policy, so neither is completable from here.
	if len(args) == 1 && (args[0] == "create" || args[0] == "list" || args[0] == "ls") {
		args = append(args, c.wantID("backup "+args[0], nil)...)
	}
	if len(args) < 2 {
		fmt.Fprintln(c.stderr, backupUsage)
		return ExitUsage
	}
	cl, err := c.client()
	if err != nil {
		return c.fail(err)
	}
	sub, id, rest := args[0], args[1], args[2:]
	base := "/v1/sandboxes/" + id + "/backups"

	switch sub {
	case "create":
		return backupCreate(c, cl, id, rest)

	case "list", "ls":
		if len(rest) != 0 {
			fmt.Fprintln(c.stderr, backupUsage)
			return ExitUsage
		}
		list, err := backupList(cl, id)
		if err != nil {
			return c.fail(err)
		}
		if c.jsonOut {
			printJSON(c.stdout, list)
			return ExitOK
		}
		rows := [][]string{{"ID", "STATUS", "KIND", "SIZE", "CREATED", "DETAIL"}}
		for _, b := range list {
			detail := b.Error
			if detail == "" && b.Deduped {
				detail = "deduped on " + b.ParentID
			}
			rows = append(rows, []string{b.ID, b.Status, b.Kind, mib(b.Bytes), stamp(b.CreatedAt), detail})
		}
		table(c.stdout, rows)
		return ExitOK

	case "rm", "delete":
		// The stored copy is gone, and it is the only one — same rule as every
		// other destructive verb here.
		if len(rest) != 1 {
			fmt.Fprintln(c.stderr, backupUsage)
			return ExitUsage
		}
		if !confirmed {
			fmt.Fprintf(c.stderr, "pilldrops backup rm: %s of %s is deleted and nothing restores it — re-run with --confirm\n", rest[0], id)
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
		// The API refuses an unconfirmed restore ({"confirm":true}); the flag
		// carries that intent rather than the CLI asserting it for the user.
		if len(rest) != 1 {
			fmt.Fprintln(c.stderr, backupUsage)
			return ExitUsage
		}
		name := rest[0]
		if !confirmed {
			fmt.Fprintf(c.stderr, "pilldrops backup restore: %s's disk is overwritten with %s and every write since is lost — re-run with --confirm\n", id, name)
			return ExitUsage
		}
		var out map[string]any
		if err := cl.JSON("POST", base+"/"+url.PathEscape(name)+"/restore",
			map[string]any{"confirm": true}, &out); err != nil {
			return c.fail(err)
		}
		if c.jsonOut {
			printJSON(c.stdout, out)
		} else if !c.quiet {
			fmt.Fprintf(c.stdout, "%s restored from %s in %sms\n", id, name, str(out["restore_ms"]))
		}
		return ExitOK

	case "schedule":
		return backupSchedule(c, cl, id, rest)
	}
	fmt.Fprintln(c.stderr, backupUsage)
	return ExitUsage
}

func backupList(cl *Client, id string) ([]backup, error) {
	var page struct {
		Backups []backup `json:"backups"`
	}
	err := cl.JSON("GET", "/v1/sandboxes/"+id+"/backups", nil, &page)
	return page.Backups, err
}

func backupCreate(c *cmdCtx, cl *Client, id string, args []string) int {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	noWait := fs.Bool("no-wait", false, "print the pending backup and return, instead of waiting for the upload")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(c.stderr, backupUsage)
		return ExitUsage
	}
	var started backup
	if err := cl.JSON("POST", "/v1/sandboxes/"+id+"/backups", nil, &started); err != nil {
		return c.fail(err)
	}
	if *noWait {
		if c.jsonOut {
			printJSON(c.stdout, started)
		} else if !c.quiet {
			fmt.Fprintf(c.stdout, "%s %s (guest paused %dms) — poll with `pilldrops backup list %s`\n",
				started.ID, started.Status, started.PausedMs, id)
		}
		return ExitOK
	}
	if !c.quiet && !c.jsonOut {
		// The capture is done (the guest ran again the moment the route
		// answered); what remains is the upload, and that is the slow part.
		fmt.Fprintf(c.stdout, "%s captured (guest paused %dms), uploading — this takes minutes on a large disk\n",
			started.ID, started.PausedMs)
	}
	done, err := awaitBackup(cl, id, started.ID)
	if err != nil {
		return c.fail(err)
	}
	if c.jsonOut {
		printJSON(c.stdout, done)
		return ExitOK
	}
	if done.Status != "ready" {
		fmt.Fprintf(c.stderr, "backup %s ended %s: %s\n", done.ID, done.Status, done.Error)
		return ExitAPI
	}
	if !c.quiet {
		fmt.Fprintf(c.stdout, "%s ready — %s, %s\n", done.ID, done.Kind, mib(done.Bytes))
	}
	return ExitOK
}

// awaitBackup polls the sandbox's backups until this one leaves `pending`.
func awaitBackup(cl *Client, id, bkp string) (backup, error) {
	deadline := time.Now().Add(backupPollFor)
	for {
		list, err := backupList(cl, id)
		if err != nil {
			return backup{}, err
		}
		for _, b := range list {
			if b.ID == bkp && b.Status != "pending" {
				return b, nil
			}
		}
		if time.Now().After(deadline) {
			return backup{}, fmt.Errorf("timed out after %s waiting for backup %s (it may still finish — `pilldrops backup list %s`)",
				backupPollFor, bkp, id)
		}
		time.Sleep(backupPollEvery)
	}
}

// backupSchedule reads or sets the sandbox's backup job. With no flags it only
// reports: the schedule rides the config route, and a bare POST there would be
// a write nobody asked for.
func backupSchedule(c *cmdCtx, cl *Client, id string, args []string) int {
	fs := flag.NewFlagSet("schedule", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	every := fs.String("every", "", "period between backups, e.g. 24h or 30m (minimum 15m)")
	cron := fs.String("cron", "", "5-field cron expression, instead of --every")
	retain := fs.String("retain", "", "how long these backups are kept, e.g. 7d (needs a schedule)")
	off := fs.Bool("off", false, "remove the schedule")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(c.stderr, backupUsage)
		return ExitUsage
	}
	if *off && (*every != "" || *cron != "") {
		fmt.Fprintln(c.stderr, "pilldrops backup schedule: --off takes no --every or --cron")
		return ExitUsage
	}
	if *off {
		*every = "off"
	}
	if *every != "" || *cron != "" || *retain != "" {
		sched := map[string]any{"period": *every, "cron": *cron, "retention": *retain}
		if err := cl.JSON("POST", "/v1/sandboxes/"+id+"/config", map[string]any{"backups": sched}, nil); err != nil {
			return c.fail(err)
		}
	}
	// Always read back: the config POST answers with the environment view and
	// says nothing about the schedule, and the server rewrites --every into a
	// cron expression — so what was asked for is not what is stored.
	var cfg struct {
		Backups map[string]any `json:"backups"`
	}
	if err := cl.JSON("GET", "/v1/sandboxes/"+id+"/config", nil, &cfg); err != nil {
		return c.fail(err)
	}
	if c.jsonOut {
		printJSON(c.stdout, cfg.Backups)
		return ExitOK
	}
	if cfg.Backups == nil {
		fmt.Fprintf(c.stdout, "%s has no backup schedule\n", id)
		return ExitOK
	}
	rows := [][]string{}
	for _, k := range []string{"cron", "enabled", "retention", "last_status"} {
		if v, ok := cfg.Backups[k]; ok {
			rows = append(rows, []string{k, str(v)})
		}
	}
	if v, ok := cfg.Backups["next_run_at"]; ok {
		rows = append(rows, []string{"next_run", stamp(int64(toFloat(v)))})
	}
	table(c.stdout, rows)
	return ExitOK
}

// stamp renders a unix second for a table cell; 0 is "never happened".
func stamp(sec int64) string {
	if sec <= 0 {
		return "-"
	}
	return time.Unix(sec, 0).Format("2006-01-02 15:04")
}

// toFloat reads a JSON number out of an any.
func toFloat(v any) float64 {
	f, _ := v.(float64)
	return f
}
