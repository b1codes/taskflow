package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"

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

		if !report.Enabled {
			fmt.Println("No completeness manifest (.agents/completeness.yaml or .taskflow/completeness.yaml) in this session's project.")
			return nil
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
		for _, w := range report.Warnings {
			fmt.Printf("  ! %s\n", w)
		}
		return nil
	},
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
