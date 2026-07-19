package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/b1codes/taskflow/internal/gitctx"
	"github.com/b1codes/taskflow/internal/session"
	"github.com/b1codes/taskflow/internal/store"
	"github.com/spf13/cobra"
)

type gitCapturerImpl struct{}

func (g *gitCapturerImpl) Capture(projectPath string) (*session.GitContext, error) {
	return gitctx.Capture(projectPath)
}

var startCmd = &cobra.Command{
	Use:   "start [task_id] [project_path]",
	Short: "Start or resume a coding session",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		taskID := args[0]
		projectPath := args[1]

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

		res, err := srv.Start(cmd.Context(), taskID, projectPath, cfg.Git.AutoContext)
		if err != nil {
			return err
		}

		if JSONOutput() {
			encoder := json.NewEncoder(os.Stdout)
			encoder.SetIndent("", "  ")
			if err := encoder.Encode(res.AgenticContract); err != nil {
				return fmt.Errorf("failed to encode JSON: %w", err)
			}
		} else {
			fmt.Printf("✓ Session started: %s\n", res.Session.ID)
			fmt.Printf("  Task ID:      %s\n", res.Session.TaskID)
			fmt.Printf("  Task Name:    %s\n", res.Session.TaskName)
			fmt.Printf("  Project Path: %s\n", res.Session.ProjectPath)
			if res.Session.GitBranch != "" {
				fmt.Printf("  Git Branch:   %s\n", res.Session.GitBranch)
			}
		}

		return nil
	},
}

func init() {
	rootCmd.AddCommand(startCmd)
}
