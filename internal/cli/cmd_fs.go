package cli

import (
	"errors"
	"fmt"
	"net/url"
	"time"
)

// `cp` moves bytes in and out; everything else about the sandbox's filesystem
// lives here, under the shell spellings people already type. Paths are the ones
// `cp` takes: relative and ~/ are the sandbox home.
const fsUsage = `usage: pilldrops fs ls <id> [path]
       pilldrops fs stat <id> <path>
       pilldrops fs mkdir <id> <path>
       pilldrops fs mv <id> <src> <dst>
       pilldrops fs rm <id> <path>`

// dirEntry mirrors the filesystem/list wire shape.
type dirEntry struct {
	Name     string    `json:"name"`
	IsDir    bool      `json:"is_dir"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

func cmdFS(c *cmdCtx, args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(c.stderr, fsUsage)
		return ExitUsage
	}
	cl, err := c.client()
	if err != nil {
		return c.fail(err)
	}
	sub, id, rest := args[0], args[1], args[2:]
	base := "/v1/sandboxes/" + id + "/filesystem/"

	// Every failure below is about a path inside a sandbox the request already
	// found, so a 404 is the path and not the id. The generic hint sent people
	// to `sandbox ls` to check the one thing that had been right.
	fail := func(err error) int {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == 404 {
			return c.fail(pathError{err, "that path is not in the sandbox — " +
				"`pilldrops fs ls " + id + "` lists the home directory"})
		}
		return c.fail(err)
	}

	// ok reports a {"status":"ok"} mutation the same way for all three of them.
	ok := func(path string, body map[string]any, msg string) int {
		if err := cl.JSON("POST", base+path, body, nil); err != nil {
			return fail(err)
		}
		if c.jsonOut {
			printJSON(c.stdout, map[string]any{"status": "ok", "sandbox": id})
		} else if !c.quiet {
			fmt.Fprintln(c.stdout, msg)
		}
		return ExitOK
	}

	switch sub {
	case "ls", "list":
		if len(rest) > 1 {
			fmt.Fprintln(c.stderr, fsUsage)
			return ExitUsage
		}
		path := "."
		if len(rest) == 1 {
			path = homePath(rest[0])
		}
		var page struct {
			Entries []dirEntry `json:"entries"`
		}
		if err := cl.JSON("GET", base+"list?path="+url.QueryEscape(path), nil, &page); err != nil {
			return fail(err)
		}
		if c.jsonOut {
			printJSON(c.stdout, page.Entries)
			return ExitOK
		}
		for _, e := range page.Entries {
			name := e.Name
			if e.IsDir {
				name += "/"
			}
			// Not every runtime's filesystem reports a modification time; a
			// rendered year 1 would read as a real date.
			when := "-"
			if !e.Modified.IsZero() {
				when = e.Modified.Format("2006-01-02 15:04")
			}
			fmt.Fprintf(c.stdout, "%10d  %16s  %s\n", e.Size, when, name)
		}
		return ExitOK

	case "stat":
		if len(rest) != 1 {
			fmt.Fprintln(c.stderr, fsUsage)
			return ExitUsage
		}
		var out map[string]any
		if err := cl.JSON("GET", base+"stat?path="+url.QueryEscape(homePath(rest[0])), nil, &out); err != nil {
			return fail(err)
		}
		if c.jsonOut {
			printJSON(c.stdout, out)
			return ExitOK
		}
		rows := [][]string{}
		for _, k := range []string{"name", "is_dir", "size", "mode", "modified"} {
			if v, ok := out[k]; ok {
				rows = append(rows, []string{k, str(v)})
			}
		}
		table(c.stdout, rows)
		return ExitOK

	case "mkdir":
		if len(rest) != 1 {
			fmt.Fprintln(c.stderr, fsUsage)
			return ExitUsage
		}
		p := homePath(rest[0])
		return ok("mkdir", map[string]any{"path": p}, "mkdir "+id+":"+p)

	case "mv", "move":
		if len(rest) != 2 {
			fmt.Fprintln(c.stderr, fsUsage)
			return ExitUsage
		}
		src, dst := homePath(rest[0]), homePath(rest[1])
		return ok("move", map[string]any{"src": src, "dst": dst}, src+" -> "+dst)

	case "rm", "remove":
		if len(rest) != 1 {
			fmt.Fprintln(c.stderr, fsUsage)
			return ExitUsage
		}
		p := homePath(rest[0])
		return ok("remove", map[string]any{"path": p}, "removed "+id+":"+p)
	}
	fmt.Fprintln(c.stderr, fsUsage)
	return ExitUsage
}
