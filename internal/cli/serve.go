package cli

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/b1codes/taskflow/internal/server"
	"github.com/b1codes/taskflow/internal/store"
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

		errChan := make(chan error, 1)
		go func() {
			errChan <- srv.Run(ctx)
		}()

		select {
		case err := <-errChan:
			return err
		case <-ctx.Done():
			return nil
		}
	},
}

func init() {
	rootCmd.AddCommand(serveCmd)
}
