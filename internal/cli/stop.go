package cli

import (
	"fmt"
	"strings"

	"github.com/b1codes/taskflow/internal/session"
	"github.com/b1codes/taskflow/internal/store"
	"github.com/spf13/cobra"
)

var (
	stopSummary string
	stopStatus  string
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

		srv := session.NewService(st, &gitCapturerImpl{}, nil)

		targetStatus := strings.ToUpper(stopStatus)
		if targetStatus == "" {
			targetStatus = "COMPLETED"
		}

		err = srv.Stop(cmd.Context(), sessionID, stopSummary, targetStatus, cfg.Git.AutoContext)
		if err != nil {
			return err
		}

		if JSONOutput() {
			fmt.Printf("{\n  \"status\": \"ok\",\n  \"session_id\": %q,\n  \"new_status\": %q\n}\n", sessionID, targetStatus)
		} else {
			fmt.Printf("✓ Session %s stopped. Status set to: %s\n", sessionID, targetStatus)
		}

		return nil
	},
}

func init() {
	stopCmd.Flags().StringVarP(&stopSummary, "summary", "s", "", "Markdown summary of final achievements before stopping")
	stopCmd.Flags().StringVarP(&stopStatus, "status", "t", "completed", "Target status: completed, paused, archived")

	rootCmd.AddCommand(stopCmd)
}
