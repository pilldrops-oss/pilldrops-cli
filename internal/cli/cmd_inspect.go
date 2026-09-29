package cli

import "fmt"

func cmdPs(c *cmdCtx, args []string) int {
	args = c.wantID("ps", args)
	if len(args) != 1 {
		fmt.Fprintln(c.stderr, "usage: pilldrops ps <id>")
		return ExitUsage
	}
	cl, err := c.client()
	if err != nil {
		return c.fail(err)
	}
	// The node serves the process list as a bare JSON array of {pid,cmd}. (An
	// earlier CLI expected a {"processes":[…]} envelope and failed to unmarshal.)
	var procs []map[string]any
	if err := cl.JSON("GET", "/v1/sandboxes/"+args[0]+"/process/list", nil, &procs); err != nil {
		return c.fail(err)
	}
	if c.jsonOut {
		printJSON(c.stdout, procs)
		return ExitOK
	}
	rows := [][]string{{"PID", "CMD"}}
	for _, p := range procs {
		rows = append(rows, []string{str(p["pid"]), str(p["cmd"])})
	}
	table(c.stdout, rows)
	return ExitOK
}

func cmdMetrics(c *cmdCtx, args []string) int {
	args = c.wantID("metrics", args)
	if len(args) != 1 {
		fmt.Fprintln(c.stderr, "usage: pilldrops metrics <id>")
		return ExitUsage
	}
	cl, err := c.client()
	if err != nil {
		return c.fail(err)
	}
	var out map[string]any
	if err := cl.JSON("GET", "/v1/sandboxes/"+args[0]+"/metrics", nil, &out); err != nil {
		return c.fail(err)
	}
	if c.jsonOut {
		printJSON(c.stdout, out)
		return ExitOK
	}
	rows := [][]string{}
	// Allocation + cumulative billing counters, then live gauges (cpu_pct,
	// mem_used_mib, disk_used_mb, disk_limit_mb) when the node reports them.
	for _, k := range []string{
		"state", "vcpu", "mem_mib", "uptime_seconds", "vcpu_seconds", "ram_gb_seconds",
		"cpu_pct", "mem_used_mib", "disk_used_mb", "disk_limit_mb",
	} {
		if v, ok := out[k]; ok {
			rows = append(rows, []string{k, str(v)})
		}
	}
	table(c.stdout, rows)
	return ExitOK
}

func cmdUsage(c *cmdCtx, _ []string) int {
	cl, err := c.client()
	if err != nil {
		return c.fail(err)
	}
	var out map[string]any
	if err := cl.JSON("GET", "/v1/usage", nil, &out); err != nil {
		return c.fail(err)
	}
	// --json is the raw contract (includes the per-second points series);
	// the default is a clean money+compute summary, not a wall of JSON.
	if c.jsonOut {
		printJSON(c.stdout, out)
		return ExitOK
	}
	money := func(v any) string { return fmt.Sprintf("$%.4f", numOf(v)) }
	rows := [][]string{
		{"cost", money(out["cost_usd"])},
		{"vcpu_hours", str(out["vcpu_hours"])},
		{"ram_gb_hours", str(out["ram_gb_hours"])},
	}
	if b, ok := out["credits_enabled"].(bool); ok && b {
		rows = append(rows,
			[]string{"credits_spent", money(out["credits_spent"])},
			[]string{"credits_balance", money(out["credits_balance"])},
		)
	}
	table(c.stdout, rows)
	return ExitOK
}
