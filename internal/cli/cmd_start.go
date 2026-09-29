package cli

import "fmt"

// cmdStart registers and launches a long-lived server on an existing sandbox:
//
//	pilldrops start <id> [--port N] -- <cmd...>
//
// Unlike `exec`, the command is recorded as the sandbox's start command, so it is
// relaunched on resume/restart and survives the idle-pause cycle. This is the
// deploy step for an uploaded app: create → cp → build → start. The command's
// port (default 8000) becomes the sandbox's preview port.
func cmdStart(c *cmdCtx, args []string) int {
	dash := -1
	for i, a := range args {
		if a == "--" {
			dash = i
			break
		}
	}
	if dash < 1 || dash == len(args)-1 {
		fmt.Fprintln(c.stderr, "usage: pilldrops start <id> [--port N] -- <cmd...>")
		return ExitUsage
	}
	id := args[0]
	port := 0
	opts := args[1:dash]
	for i := 0; i < len(opts); i++ {
		switch opts[i] {
		case "--port":
			if i+1 >= len(opts) {
				fmt.Fprintln(c.stderr, "pilldrops start: --port needs a value")
				return ExitUsage
			}
			if _, err := fmt.Sscanf(opts[i+1], "%d", &port); err != nil {
				fmt.Fprintln(c.stderr, "pilldrops start: --port must be a number")
				return ExitUsage
			}
			i++
		default:
			fmt.Fprintf(c.stderr, "pilldrops start: unknown option %q\n", opts[i])
			return ExitUsage
		}
	}
	cl, err := c.client()
	if err != nil {
		return c.fail(err)
	}
	var out struct {
		Started string `json:"started"`
		Port    int    `json:"port"`
	}
	req := map[string]any{"cmd": joinCmd(args[dash+1:]), "port": port}
	if err := cl.JSON("POST", "/v1/sandboxes/"+id+"/start", req, &out); err != nil {
		return c.fail(err)
	}
	if c.jsonOut {
		printJSON(c.stdout, out)
	} else if !c.quiet {
		fmt.Fprintf(c.stdout, "started on :%d — %s\n", out.Port, out.Started)
		fmt.Fprintln(c.stdout, "  (survives idle-pause/resume; your app must listen on $PORT)")
	}
	return ExitOK
}
