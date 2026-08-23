package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// printReport renders the human form of `state show`.
//
// The ordering is the diagnostic order: what the operator asked for, what is
// actually true, what it costs, what is pending, and finally what was wrong with
// the documents. Repairs come last because they are the longest section and the
// least often needed — but they are never hidden, since a silently repaired
// document is how bug 3 stayed invisible for as long as it did.
func printReport(out io.Writer, in reportInput) {
	w := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
	row := func(k string, v any) { fmt.Fprintf(w, "  %s\t%v\n", k, v) }

	fmt.Fprintf(w, "source\t%s\n", in.Source)
	if in.Head != "" {
		fmt.Fprintf(w, "head\t%s\n", short(in.Head))
	}
	fmt.Fprintf(w, "timezone\t%s (now %s)\n", in.Loc, state.Now(in.Loc))
	if in.Legacy {
		fmt.Fprintf(w, "mode\tv1 import — no v2 state has been published yet\n")
	}

	c := in.Controller
	fmt.Fprintln(w, "\ncontroller")
	row("intent", c.Intent)
	row("status", fmt.Sprintf("%s (%s, %s)", c.Status, since(c.Since), billing(c.Status)))
	row("updated", c.UpdatedAt)
	if c.Lease != nil {
		row("lease", fmt.Sprintf("dseq %s gseq %d oseq %d provider %s (up %s)",
			c.Lease.DSeq, c.Lease.GSeq, c.Lease.OSeq, orDash(c.Lease.Provider), since(c.Lease.CreatedAt)))
	} else {
		row("lease", "none")
	}
	if c.Endpoint.Ready() {
		// Addr(), not IP. A shared-endpoint lease has no IP at all — the address is
		// the provider's hostname — so printing the IP field rendered the live world
		// as ":30975", which reads like a broken endpoint rather than a working one
		// on a borrowed name. Ready() had already said otherwise; only this line
		// disagreed.
		row("endpoint", fmt.Sprintf("%s:%d", c.Endpoint.Addr(), c.Endpoint.GamePort))
	} else {
		row("endpoint", "not ready")
	}
	if p := c.Price; p.USDPerDay > 0 || p.USDPerHour > 0 || p.AmountPerBlock > 0 {
		row("price", fmt.Sprintf("%.4f USD/h · %.2f USD/day · %d %s/block",
			p.USDPerHour, p.USDPerDay, p.AmountPerBlock, p.Denom))
	}
	if u := c.URLs.Base(); u != "" {
		row("url", u)
	}
	if c.RestoreTarget != "" {
		// Provenance, not decoration: pinned means the next backup will not move
		// this, and that is the difference between the world an operator chose and
		// the newest one.
		if c.RestorePinned {
			row("restore target", c.RestoreTarget+" (pinned)")
		} else {
			row("restore target", c.RestoreTarget)
		}
	}
	if c.StopAt != nil {
		row("stop at", fmt.Sprintf("%s (%s)", c.StopAt, until(c.StopAt.Time)))
	}
	if c.LastError != "" {
		row("last error", c.LastError)
	}
	if n := len(c.ProcessedSHAs); n > 0 {
		row("processed shas", fmt.Sprintf("%d of %d", n, state.ProcessedSHACap))
	}

	if a := in.Agent; a != nil {
		fmt.Fprintln(w, "\nagent")
		row("phase", fmt.Sprintf("%s (%s)", a.Phase, since(a.Since)))
		// The distinction this line makes is the whole of bug 1. An unmeasured
		// count and an empty server are different facts, and v1 printed both as 0.
		if a.PlayersKnown() {
			row("players", fmt.Sprintf("%d (measured %s)", a.PlayersCount, since(a.PlayersAt)))
		} else {
			row("players", "unknown — not measured")
		}
		row("restarts", a.Restarts)
		row("liveness", fmt.Sprintf("%s (%s)", a.LivenessAt, since(a.LivenessAt)))
		if b := a.Backup; b != nil {
			line := fmt.Sprintf("%s request %s", b.State, short(b.RequestID))
			if b.Name != "" {
				line += fmt.Sprintf(" · %s (%s)", b.Name, bytesHuman(b.Size))
			}
			if b.Error != "" {
				line += " · " + b.Error
			}
			row("backup", line)
		}
		if a.LastError != "" {
			row("last error", a.LastError)
		}
	} else if !in.Legacy {
		fmt.Fprintln(w, "\nagent\n  (never published)")
	}

	idx := in.Backups
	fmt.Fprintf(w, "\nbackups\t%d, %s\n", len(idx.Items), bytesHuman(idx.TotalBytes()))
	for _, b := range idx.Items {
		flags := []string{}
		if b.DownloadedAt.Zero() {
			// This is the one that matters under the operator's chosen durability
			// model: an undownloaded backup exists only inside a lease that will
			// eventually end.
			flags = append(flags, "not downloaded")
		}
		if b.SHA256 == "" {
			flags = append(flags, "no checksum")
		}
		row(b.Name, strings.TrimSpace(fmt.Sprintf("%s  %s  %s",
			pad(bytesHuman(b.Size), 9), b.CreatedAt, strings.Join(flags, ", "))))
	}

	fmt.Fprintf(w, "\ntriggers\t%d pending\n", len(in.Triggers))
	for _, t := range in.Triggers {
		row(t.Name, firstLine(t.Body))
	}

	reps := append(repairStrings(in.Repairs), repairStrings(in.AgentRepairs)...)
	fmt.Fprintf(w, "\nrepairs\t%d\n", len(reps))
	for _, r := range reps {
		fmt.Fprintf(w, "  %s\n", r)
	}
	w.Flush()

	// Fatal repairs go to stderr as well, so a `state show` in a pipeline or a
	// CI log surfaces them even when the report itself is being captured.
	if in.Repairs.Fatal() || in.AgentRepairs.Fatal() {
		fmt.Fprintln(os.Stderr,
			"pzctl: WARNING — a document failed to parse and was replaced with safe defaults")
	}
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func pad(s string, n int) string {
	for len(s) < n {
		s += " "
	}
	return s
}

// since renders an age, or "never" for an unstamped field. A zero stamp printed
// as a date is how v1 reported "1970-01-01" as if it were an observation.
func since(s state.Stamp) string {
	if s.Zero() {
		return "never"
	}
	return dur(s.Age()) + " ago"
}

func until(t time.Time) string {
	d := time.Until(t)
	if d < 0 {
		return "overdue by " + dur(-d)
	}
	return "in " + dur(d)
}

func dur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func billing(s state.Status) string {
	if s.Billing() {
		return "billing"
	}
	return "not billing"
}

func bytesHuman(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	v, exp := float64(n), 0
	for v >= unit && exp < 4 {
		v /= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", v, "KMGT"[exp-1])
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "(empty)"
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if len(s) > 72 {
		s = s[:72] + "…"
	}
	return s
}
