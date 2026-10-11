package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/b1codes/taskflow/internal/session"
	"github.com/b1codes/taskflow/internal/store"
	"github.com/spf13/cobra"
)

var attestEvidence string

var attestCmd = &cobra.Command{
	Use:   "attest [session_id] [obligation_id]",
	Short: "Attest that a completeness obligation is met, with evidence",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
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

		res, err := srv.Attest(cmd.Context(), args[0], args[1], attestEvidence)
		if err != nil {
			return err
		}

		if JSONOutput() {
			encoder := json.NewEncoder(os.Stdout)
			encoder.SetIndent("", "  ")
			if err := encoder.Encode(res); err != nil {
				return fmt.Errorf("failed to encode JSON: %w", err)
			}
			return nil
		}
		fmt.Printf("✓ Attested: %s (%s)\n", res.Obligation.Describe, res.Obligation.Status)
		printObligationSummary(res.Summary, res.StillOpen)
		return nil
	},
}

// printObligationSummary prints the session's obligation counts and what is
// still open.
func printObligationSummary(sum session.CompletenessSummary, open []session.ObligationView) {
	fmt.Printf("  open: %d · satisfied: %d · attested: %d · waived: %d\n", sum.Open, sum.Satisfied, sum.Attested, sum.Waived)
	for _, o := range open {
		fmt.Printf("  ○ %s [%s]\n", o.Describe, o.ObligationID)
	}
}

func init() {
	attestCmd.Flags().StringVarP(&attestEvidence, "evidence", "e", "", "What shows the obligation is met (e.g. the spec and test cases that cover it)")
	_ = attestCmd.MarkFlagRequired("evidence")

	rootCmd.AddCommand(attestCmd)
}
