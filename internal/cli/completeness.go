package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/b1codes/taskflow/internal/session"
	"github.com/b1codes/taskflow/internal/store"
	"github.com/spf13/cobra"
)

var completenessCmd = &cobra.Command{
	Use:   "completeness [session_id]",
	Short: "Report what the session still owes under the project's completeness manifest",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		sessionID := args[0]

		cfg := ConfigFromContext(cmd.Context())
		if cfg == nil {
			return fmt.Errorf("config not found in context")
		}

		st, err := store.New(cfg.DBPath())
		if err != nil {
			return fmt.Errorf("failed to open database: %w", err)
		}
		defer st.Close()

		srv := newSessionService(st, cfg)

		report, err := srv.Completeness(cmd.Context(), sessionID)
		if err != nil {
			return err
		}

		if JSONOutput() {
			encoder := json.NewEncoder(os.Stdout)
			encoder.SetIndent("", "  ")
			if err := encoder.Encode(report); err != nil {
				return fmt.Errorf("failed to encode JSON: %w", err)
			}
			return nil
		}

		printReport(report)
		return nil
	},
}

// printReport prints a completeness report as text.
func printReport(report *session.CompletenessReport) {
	if !report.Enabled {
		fmt.Println("No completeness manifest (.agents/completeness.yaml or .taskflow/completeness.yaml) in this session's project.")
		return
	}
	fmt.Printf("Completeness for session %s (%s)\n", report.SessionID, report.Manifest)
	if report.Error != "" {
		fmt.Printf("  ✗ %s\n", report.Error)
	}
	sum := report.Summary
	fmt.Printf("  open: %d · satisfied: %d · attested: %d · waived: %d\n", sum.Open, sum.Satisfied, sum.Attested, sum.Waived)
	for _, o := range report.Obligations {
		fmt.Printf("  %s %s [%s, %s]\n", statusMark(o.Status), o.Describe, o.Rule+"/"+o.Requirement, o.Severity)
		if o.Status == session.ObligationOpen && o.Expected != "" {
			fmt.Printf("      expected: %s\n", o.Expected)
		}
		printCommandView(o.Command)
	}
	for _, c := range report.Commands {
		mark := "✗"
		if c.Passed {
			mark = "✓"
		}
		fmt.Printf("  %s ran `%s` in %s", mark, c.Command, c.Root)
		if !c.Passed {
			fmt.Printf(" (exit %d)", c.ExitCode)
			if c.Error != "" {
				fmt.Printf(": %s", c.Error)
			}
		}
		fmt.Println()
	}
	for _, s := range report.Stacks {
		for _, name := range sortedCommandNames(s.Commands) {
			line := "(unresolved)"
			if c := s.Commands[name]; c != nil {
				line = *c
			}
			fmt.Printf("  stack %s (%s) %s: %s\n", s.ID, s.Root, name, line)
		}
	}
	if report.Enabled && !report.CommandsEnabled {
		fmt.Println("  command checks: off (completeness.allow_commands)")
	}
	for _, w := range report.Warnings {
		fmt.Printf("  ! %s\n", w)
	}
}

// printCommandView prints an obligation's command check, with the end of a
// failed run's output.
func printCommandView(c *session.CommandView) {
	if c == nil {
		return
	}
	fmt.Printf("      command: `%s` in %s: %s\n", c.Command, c.Root, c.Result)
	if c.Output == "" {
		return
	}
	lines := strings.Split(strings.TrimRight(c.Output, "\n"), "\n")
	if len(lines) > 10 {
		lines = lines[len(lines)-10:]
	}
	for _, l := range lines {
		fmt.Printf("        | %s\n", l)
	}
}

func printContractCompleteness(c *session.ContractCompleteness) {
	if c.Error != "" {
		fmt.Printf("  ✗ completeness manifest error: %s\n", c.Error)
		return
	}
	sum := c.Summary
	fmt.Printf("  Completeness: %s (gate: %s) · open: %d · satisfied: %d · attested: %d · waived: %d\n",
		c.Manifest, c.Gate, sum.Open, sum.Satisfied, sum.Attested, sum.Waived)
	for _, o := range c.Inherited {
		fmt.Printf("  %s inherited: %s (%s) [%s]\n", statusMark(o.Status), o.Describe, o.Status, o.ObligationID)
	}
	for _, o := range c.Open {
		fmt.Printf("  ○ %s [%s]\n", o.Describe, o.ObligationID)
	}
	if p := c.SuggestedPlan; p != nil {
		fmt.Printf("  ? %s: consider `taskflow plan` for %s, or decline with --decline\n", p.Reason, strings.Join(p.Rules, ", "))
	}
}

func printCheckpointCompleteness(c *session.CheckpointCompleteness) {
	if c.Error != "" {
		fmt.Printf("  ✗ completeness manifest error: %s\n", c.Error)
	}
	for _, o := range c.NewlyOpen {
		fmt.Printf("  ○ new: %s", o.Describe)
		if o.Expected != "" {
			fmt.Printf(" (expected: %s)", o.Expected)
		}
		fmt.Println()
	}
	for _, d := range c.NewlySatisfied {
		fmt.Printf("  ✓ done: %s\n", d)
	}
	for _, d := range c.StillOpen {
		fmt.Printf("  ○ still open: %s\n", d)
	}
	if c.PlanReminder != "" {
		fmt.Printf("  ? %s\n", c.PlanReminder)
	}
}

func statusMark(s session.ObligationStatus) string {
	switch s {
	case session.ObligationOpen:
		return "○"
	case session.ObligationWaived:
		return "–"
	default:
		return "✓"
	}
}

func sortedCommandNames(m map[string]*string) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func init() {
	rootCmd.AddCommand(completenessCmd)
}
