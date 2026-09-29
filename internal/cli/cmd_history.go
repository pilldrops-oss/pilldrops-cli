package cli

import (
	"fmt"
	"strconv"
	"time"
)

// cmdHistory lists a customer's destroyed sandboxes (with per-pod charge +
// runtime), or shows one record with its logs. Mirrors the dashboard History
// tab from the CLI:
//
//	pilldrops history [--limit N]   list past sandboxes, newest first
//	pilldrops history <id>          one record: charge, runtime, logs
func cmdHistory(c *cmdCtx, args []string) int {
	limit := 0
	id := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--limit", "-n":
			if i+1 < len(args) {
				i++
				limit, _ = strconv.Atoi(args[i])
			}
		default:
			if len(args[i]) > 0 && args[i][0] != '-' {
				id = args[i]
			}
		}
	}
	cl, err := c.client()
	if err != nil {
		return c.fail(err)
	}
	if id != "" {
		return historyDetail(c, cl, id)
	}
	return historyList(c, cl, limit)
}

func historyList(c *cmdCtx, cl *Client, limit int) int {
	path := "/v1/history"
	if limit > 0 {
		path += "?limit=" + strconv.Itoa(limit)
	}
	var out struct {
		History []map[string]any `json:"history"`
	}
	if err := cl.JSON("GET", path, nil, &out); err != nil {
		return c.fail(err)
	}
	if c.jsonOut {
		printJSON(c.stdout, out.History)
		return ExitOK
	}
	if len(out.History) == 0 {
		fmt.Fprintln(c.stdout, "no past sandboxes yet")
		return ExitOK
	}
	rows := [][]string{{"ID", "TEMPLATE", "RAN", "CHARGE", "DESTROYED"}}
	for _, r := range out.History {
		rows = append(rows, []string{
			str(r["id"]), str(r["template"]),
			fmtDur(numOf(r["duration_secs"])),
			histCharge(r),
			fmtWhen(numOf(r["destroyed_at"])),
		})
	}
	table(c.stdout, rows)
	return ExitOK
}

func historyDetail(c *cmdCtx, cl *Client, id string) int {
	var rec map[string]any
	if err := cl.JSON("GET", "/v1/history/"+id, nil, &rec); err != nil {
		return c.fail(err)
	}
	if c.jsonOut {
		printJSON(c.stdout, rec)
		return ExitOK
	}
	rows := [][]string{
		{"id", str(rec["id"])},
		{"template", str(rec["template"])},
		{"node", str(rec["node"])},
		{"exit", str(rec["exit_state"])},
		{"created", fmtWhen(numOf(rec["created_at"]))},
		{"destroyed", fmtWhen(numOf(rec["destroyed_at"]))},
		{"ran", fmtDur(numOf(rec["duration_secs"]))},
		{"charge", histCharge(rec)},
		{"vcpu_seconds", trim1(numOf(rec["vcpu_seconds"]))},
		{"ram_gb_seconds", trim1(numOf(rec["ram_gb_seconds"]))},
	}
	table(c.stdout, rows)
	if logs, _ := rec["logs"].(string); logs != "" {
		fmt.Fprintln(c.stdout, "\nlogs:")
		fmt.Fprintln(c.stdout, logs)
	}
	return ExitOK
}

// histCharge renders a pod's dollar cost the same way the dashboard does: a real
// metered charge, "<$0.0001" for a tiny non-zero one, or "—" when the record
// carries no metered usage (a legacy pod from before per-pod metering).
func histCharge(r map[string]any) string {
	cost := numOf(r["cost_usd"])
	metered := numOf(r["vcpu_seconds"]) > 0 || numOf(r["ram_gb_seconds"]) > 0
	if !metered && cost == 0 {
		return "—"
	}
	if cost > 0 && cost < 0.0001 {
		return "<$0.0001"
	}
	if cost < 0.01 {
		return fmt.Sprintf("$%.4f", cost)
	}
	return fmt.Sprintf("$%.2f", cost)
}

func fmtDur(secs float64) string {
	s := int(secs)
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	if s < 3600 {
		return fmt.Sprintf("%dm%ds", s/60, s%60)
	}
	return fmt.Sprintf("%dh%dm", s/3600, (s%3600)/60)
}

// fmtWhen renders a unix time as a local short timestamp; blank for zero.
func fmtWhen(unix float64) string {
	if unix <= 0 {
		return "-"
	}
	return time.Unix(int64(unix), 0).Format("Jan 2 15:04")
}

func trim1(f float64) string { return strconv.FormatFloat(f, 'f', 1, 64) }

// numOf coerces a JSON number (float64) or numeric string to float64.
func numOf(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case string:
		f, _ := strconv.ParseFloat(x, 64)
		return f
	}
	return 0
}
