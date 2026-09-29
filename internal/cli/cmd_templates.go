package cli

func cmdTemplates(c *cmdCtx, _ []string) int {
	cl, err := c.client()
	if err != nil {
		return c.fail(err)
	}
	var out []map[string]any
	if err := cl.JSON("GET", "/v1/templates", nil, &out); err != nil {
		return c.fail(err)
	}
	if c.jsonOut {
		printJSON(c.stdout, out)
		return ExitOK
	}
	rows := [][]string{{"ID", "NAME", "CPU", "MEM_MB"}}
	for _, t := range out {
		rows = append(rows, []string{str(t["id"]), str(t["name"]), str(t["cpu"]), str(t["memory_mb"])})
	}
	table(c.stdout, rows)
	return ExitOK
}
