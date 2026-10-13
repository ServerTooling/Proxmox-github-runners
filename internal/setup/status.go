package setup

import (
	"fmt"
	"io"
	"text/tabwriter"
)

// StatusReport is what `pve-runners status` reports. The CLI prints it as text; the settings page
// shows it as cards and a table.
type StatusReport struct {
	Version string      `json:"version"`
	Host    string      `json:"host"`
	Max     int         `json:"max"`
	Runners []RunnerRow `json:"runners"`
	// Pending are runners the next cleanup destroys, with the reason.
	Pending []string `json:"pending"`
	// Storage is where runner disks go; empty when it could not be found (a warning says why).
	Storage  string   `json:"storage"`
	Warnings []string `json:"warnings"`
}

type RunnerRow struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Pool   string `json:"pool"`
	Owner  string `json:"owner"`
	Age    string `json:"age"`
}

// Print writes the report as the CLI shows it.
func (r *StatusReport) Print(out io.Writer) {
	fmt.Fprintf(out, "pve-runners %s on %s: %d/%d runners\n", r.Version, r.Host, len(r.Runners), r.Max)
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tSTATUS\tPOOL\tOWNER\tAGE")
	for _, g := range r.Runners {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", g.ID, g.Name, g.Status, g.Pool, g.Owner, g.Age)
	}
	w.Flush()
	for _, p := range r.Pending {
		fmt.Fprintf(out, "pending cleanup: %s\n", p)
	}
	if r.Storage != "" {
		fmt.Fprintln(out, "container storage:", r.Storage)
	}
	for _, w := range r.Warnings {
		fmt.Fprintln(out, "warning:", w)
	}
}

// UsedPercent is how full the host is, for the meter on the page.
func (r *StatusReport) UsedPercent() int {
	if r.Max <= 0 {
		return 0
	}
	return min(100, len(r.Runners)*100/r.Max)
}
