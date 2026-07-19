package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/b1codes/taskflow/internal/session"
	"github.com/b1codes/taskflow/internal/store"
	"github.com/spf13/cobra"
)

var (
	checkpointSummary string
	checkpointFiles   []string
)

var checkpointCmd = &cobra.Command{
	Use:   "checkpoint [session_id]",
	Short: "Record a progress checkpoint",
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

		cp, err := srv.Checkpoint(cmd.Context(), sessionID, checkpointSummary, checkpointFiles, cfg.Git.AutoContext)
		if err != nil {
			return err
		}

		if JSONOutput() {
			encoder := json.NewEncoder(os.Stdout)
			encoder.SetIndent("", "  ")
			if err := encoder.Encode(cp); err != nil {
				return fmt.Errorf("failed to encode JSON: %w", err)
			}
		} else {
			fmt.Printf("✓ Checkpoint recorded: %s\n", cp.ID)
		}

		return nil
	},
}

func init() {
	checkpointCmd.Flags().StringVarP(&checkpointSummary, "summary", "s", "", "Markdown summary of the progress made")
	checkpointCmd.Flags().StringSliceVarP(&checkpointFiles, "files", "f", nil, "Comma-separated list of file paths touched")
	_ = checkpointCmd.MarkFlagRequired("summary")

	rootCmd.AddCommand(checkpointCmd)
}
