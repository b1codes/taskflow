package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/b1codes/taskflow/internal/session"
	"github.com/b1codes/taskflow/internal/store"
	"github.com/spf13/cobra"
)

var (
	planRule    string
	planVars    []string
	planDecline string
)

var planCmd = &cobra.Command{
	Use:   "plan [session_id]",
	Short: "Declare expected work for a plannable completeness rule before its files exist, or decline the suggested plan",
	Example: `  taskflow plan <session_id> --rule react-screen --var feature=payments --var name=CheckoutPage
  taskflow plan <session_id> --decline "This task only changes copy; no new screens"`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var items []session.PlanItem
		if planRule != "" {
			item := session.PlanItem{RuleID: planRule, Vars: map[string]string{}}
			for _, kv := range planVars {
				k, v, ok := strings.Cut(kv, "=")
				if !ok || k == "" {
					return fmt.Errorf("--var %q must be key=value", kv)
				}
				item.Vars[k] = v
			}
			items = append(items, item)
		} else if len(planVars) > 0 {
			return fmt.Errorf("--var needs --rule")
		}

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

		res, err := srv.Plan(cmd.Context(), args[0], items, planDecline)
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
		if res.PlanState == session.PlanDeclined {
			fmt.Printf("✓ Plan declined (decision %s)\n", res.DecisionID)
			return nil
		}
		fmt.Printf("✓ Planned %d obligation(s)\n", len(res.Planned))
		for _, o := range res.Planned {
			fmt.Printf("  %s %s [%s]\n", statusMark(o.Status), o.Describe, o.ObligationID)
			if o.Status == session.ObligationOpen && o.Expected != "" {
				fmt.Printf("      expected: %s\n", o.Expected)
			}
		}
		for _, w := range res.Warnings {
			fmt.Printf("  ! %s\n", w)
		}
		return nil
	},
}

func init() {
	planCmd.Flags().StringVar(&planRule, "rule", "", "Plannable rule id from the completeness manifest")
	planCmd.Flags().StringArrayVar(&planVars, "var", nil, "Var for the rule's file pattern, as key=value (repeatable)")
	planCmd.Flags().StringVar(&planDecline, "decline", "", "Decline the suggested plan, with the reason (logged as a decision)")

	rootCmd.AddCommand(planCmd)
}
