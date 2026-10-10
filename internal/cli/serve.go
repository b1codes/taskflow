package cli

import (
	"fmt"
	"os"
	"os/signal"
	syncPkg "sync"
	"syscall"

	"github.com/b1codes/taskflow/internal/clickup"
	"github.com/b1codes/taskflow/internal/server"
	"github.com/b1codes/taskflow/internal/store"
	"github.com/b1codes/taskflow/internal/sync"
	"github.com/spf13/cobra"
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the MCP server",
	Long:  `Starts the Model Context Protocol (MCP) server over standard input/output.`,
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

		srv := server.New(st, cfg)

		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		// Start sync worker to drain enqueued ops to ClickUp in the background.
		var cuClient *clickup.Client
		if cfg.ClickUp.APIKey != "" {
			cuClient = clickup.New(cfg.ClickUp.APIKey)
		}
		worker := sync.NewWorker(st, cuClient, &cfg.Sync).WithStatusMap(cfg.ClickUp.StatusMap)

		var wg syncPkg.WaitGroup
		wg.Add(2)
		errChan := make(chan error, 2)
		go func() {
			defer wg.Done()
			errChan <- srv.Run(ctx)
		}()
		go func() {
			defer wg.Done()
			errChan <- worker.Run(ctx)
		}()

		select {
		case err := <-errChan:
			stop()
			wg.Wait()
			return err
		case <-ctx.Done():
			stop()
			wg.Wait()
			return nil
		}
	},
}

func init() {
	rootCmd.AddCommand(serveCmd)
}
