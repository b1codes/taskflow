package drift

import (
	"fmt"
	"strings"
	"text/tabwriter"
)

// Text renders the report as a table, one row per requirement, with its
// waiver clusters and signals below it.
func (r *Report) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Completeness drift report. %s\n", r.scope())
	if len(r.Requirements) == 0 {
		b.WriteString("No obligations recorded.\n")
		return b.String()
	}
	b.WriteString("\n")
	tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SOURCE\tRULE/REQUIREMENT\tSESSIONS\tDONE FIRST\tAFTER REMINDER\tATTESTED\tWAIVED\tOPEN AT STOP\tIN PROGRESS")
	for _, q := range r.Requirements {
		source := q.Source
		if source == "" {
			source = "(project)"
		}
		fmt.Fprintf(tw, "%s\t%s/%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\n", source, q.RuleID, q.RequirementID, q.Sessions,
			q.SatisfiedFirst, q.SatisfiedAfterReminder, q.Attested, q.Waived, q.OpenAtStop, q.OpenInProgress)
	}
	_ = tw.Flush()

	var notes []string
	for _, q := range r.Requirements {
		id := q.RuleID + "/" + q.RequirementID
		for _, s := range q.Signals {
			notes = append(notes, fmt.Sprintf("  ! %s: %s", id, s))
		}
		for _, c := range q.WaiverClusters {
			line := fmt.Sprintf("  – %s waived %d×: %q", id, c.Count, c.Example)
			if len(c.Terms) > 0 {
				line += " (" + strings.Join(c.Terms, ", ") + ")"
			}
			notes = append(notes, line)
		}
	}
	if len(notes) > 0 {
		b.WriteString("\n" + strings.Join(notes, "\n") + "\n")
	}
	b.WriteString("\nRun with --format b1 for suggested changes to the b1 modules.\n")
	return b.String()
}
