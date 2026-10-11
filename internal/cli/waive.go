package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/b1codes/taskflow/internal/store"
	"github.com/spf13/cobra"
)

var waiveRationale string

var waiveCmd = &cobra.Command{
	Use:   "waive [session_id] [obligation_id]",
	Short: "Waive a completeness obligation that doesn't apply, logging the rationale as a decision",
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

		res, err := srv.Waive(cmd.Context(), args[0], args[1], waiveRationale)
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
		fmt.Printf("✓ Waived: %s (decision %s)\n", res.Obligation.Describe, res.DecisionID)
		printObligationSummary(res.Summary, res.StillOpen)
		return nil
	},
}

func init() {
	waiveCmd.Flags().StringVarP(&waiveRationale, "rationale", "r", "", "Why the requirement doesn't apply to this change")
	_ = waiveCmd.MarkFlagRequired("rationale")

	rootCmd.AddCommand(waiveCmd)
}
