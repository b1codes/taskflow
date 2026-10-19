package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/b1codes/taskflow/internal/session"
	"github.com/b1codes/taskflow/internal/store"
	"github.com/spf13/cobra"
)

var (
	stopSummary          string
	stopStatus           string
	stopTargetTaskStatus string
	stopForce            bool
)

var stopCmd = &cobra.Command{
	Use:   "stop [session_id]",
	Short: "End or pause a session",
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

		targetStatus := strings.ToUpper(stopStatus)
		if targetStatus == "" {
			targetStatus = "COMPLETED"
		}

		res, err := srv.StopWithTaskStatus(cmd.Context(), sessionID, stopSummary, targetStatus, stopTargetTaskStatus, cfg.Git.AutoContext, stopForce)
		if errors.Is(err, session.ErrIncompleteSession) || errors.Is(err, session.ErrCompletenessUnchecked) {
			return fmt.Errorf("%w\nA human can complete it anyway with --force (recorded as a decision on the task)", err)
		}
		if err != nil {
			return err
		}

		if JSONOutput() {
			encoder := json.NewEncoder(os.Stdout)
			encoder.SetIndent("", "  ")
			if err := encoder.Encode(struct {
				Status string `json:"status"`
				*session.StopResult
			}{Status: "ok", StopResult: res}); err != nil {
				return fmt.Errorf("failed to encode JSON: %w", err)
			}
			return nil
		}

		fmt.Printf("✓ Session %s stopped. Status set to: %s\n", sessionID, res.Status)
		if g := res.Gate; g != nil && g.Outcome != session.GatePassed {
			switch {
			case g.Outcome == session.GateForced && g.Unchecked != "":
				fmt.Printf("  ! Completion forced without a completeness check: %s (decision %s)\n", g.Unchecked, g.DecisionID)
			case g.Outcome == session.GateForced:
				fmt.Printf("  ! Completion forced with %d required obligation(s) open (decision %s)\n", len(g.OpenRequired), g.DecisionID)
			case g.Outcome == session.GateMovedToReview && g.Unchecked != "":
				fmt.Printf("  ! Completeness couldn't be checked (%s): the ClickUp task goes to review instead of complete\n", g.Unchecked)
			case g.Outcome == session.GateMovedToReview:
				fmt.Printf("  ! %d required obligation(s) open: the ClickUp task goes to review instead of complete\n", len(g.OpenRequired))
			case g.Unchecked != "":
				fmt.Printf("  ! Completed without a completeness check: %s\n", g.Unchecked)
			default:
				fmt.Printf("  ! Completed with %d required obligation(s) open\n", len(g.OpenRequired))
			}
			for _, o := range g.OpenRequired {
				fmt.Printf("  ○ %s [%s]\n", o.Describe, o.ObligationID)
			}
		}

		return nil
	},
}

func init() {
	stopCmd.Flags().StringVarP(&stopSummary, "summary", "s", "", "Markdown summary of final achievements before stopping")
	stopCmd.Flags().StringVarP(&stopStatus, "status", "t", "completed", "Target status: completed (or in_review), paused, archived (or abandoned)")
	stopCmd.Flags().StringVar(&stopTargetTaskStatus, "target-task-status", "", "Optional ClickUp status override for the task (e.g. 'in review', 'testing/qa', 'complete')")
	stopCmd.Flags().BoolVar(&stopForce, "force", false, "Complete even when the completion gate blocks it (records a decision naming the open obligations)")

	rootCmd.AddCommand(stopCmd)
}
