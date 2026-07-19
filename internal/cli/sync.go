package cli

import (
	"fmt"

	"github.com/b1codes/taskflow/internal/clickup"
	"github.com/b1codes/taskflow/internal/store"
	"github.com/b1codes/taskflow/internal/sync"
	"github.com/spf13/cobra"
)

var syncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Drain the sync queue to ClickUp",
	Long:  `Processes and drains all pending and retryable sync operations to the ClickUp API.`,
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

		var cuClient *clickup.Client
		if cfg.ClickUp.APIKey != "" {
			cuClient = clickup.New(cfg.ClickUp.APIKey)
		} else {
			fmt.Println("Warning: CLICKUP_API_KEY is not set. Sync operations will be marked as done without hitting ClickUp API.")
		}

		worker := sync.NewWorker(st, cuClient, &cfg.Sync)

		fmt.Println("Starting sync queue drain...")
		count, err := worker.DrainOnce(cmd.Context())
		if err != nil {
			return fmt.Errorf("sync failed: %w", err)
		}

		fmt.Printf("Sync completed: %d items processed.\n", count)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(syncCmd)
}
