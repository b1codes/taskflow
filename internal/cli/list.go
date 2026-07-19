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
	listStatus  string
	listProject string
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List coding sessions",
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

		srv := session.NewService(st, &gitCapturerImpl{}, nil)

		var statuses []session.Status
		if listStatus != "" {
			parts := strings.Split(listStatus, ",")
			for _, part := range parts {
				part = strings.ToUpper(strings.TrimSpace(part))
				if part != "" {
					statuses = append(statuses, session.Status(part))
				}
			}
		} else {
			statuses = []session.Status{session.StatusActive, session.StatusPaused}
		}

		filter := session.SessionFilter{
			Status:      statuses,
			ProjectPath: listProject,
		}

		summaries, err := srv.List(cmd.Context(), filter)
		if err != nil {
			return err
		}

		if JSONOutput() {
			encoder := json.NewEncoder(os.Stdout)
			encoder.SetIndent("", "  ")
			if err := encoder.Encode(summaries); err != nil {
				return fmt.Errorf("failed to encode JSON: %w", err)
			}
		} else {
			if len(summaries) == 0 {
				fmt.Println("No sessions found.")
				return nil
			}

			fmt.Printf("%-36s %-10s %-8s %-15s %-4s %-4s %s\n", "SESSION ID", "TASK ID", "STATUS", "LAST CHECKPOINT", "SNAG", "SYNC", "TASK NAME")
			fmt.Println(strings.Repeat("-", 100))
			for _, s := range summaries {
				lastCpStr := "never"
				if !s.LastCheckpointAt.IsZero() {
					lastCpStr = s.LastCheckpointAt.Format("01-02 15:04")
				}
				fmt.Printf("%-36s %-10s %-8s %-15s %-4d %-4d %s\n",
					s.SessionID, s.TaskID, s.Status, lastCpStr, s.UnresolvedSnagCount, s.PendingSyncCount, s.TaskName)
			}
		}

		return nil
	},
}

func init() {
	listCmd.Flags().StringVarP(&listStatus, "status", "t", "active,paused", "Filter by status: active, paused, completed, archived")
	listCmd.Flags().StringVarP(&listProject, "project", "p", "", "Filter by project directory path")

	rootCmd.AddCommand(listCmd)
}
