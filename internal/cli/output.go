package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
)

// printJSON writes exactly one JSON value — the whole --json contract.
func printJSON(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// table renders rows with aligned columns; the first row is the header.
func table(w io.Writer, rows [][]string) {
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	for _, r := range rows {
		for i, cell := range r {
			if i > 0 {
				fmt.Fprint(tw, "\t")
			}
			fmt.Fprint(tw, cell)
		}
		fmt.Fprintln(tw)
	}
	_ = tw.Flush()
}
