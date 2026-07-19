package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/b1codes/taskflow/internal/config"
	"github.com/spf13/cobra"
)

type contextKey string

const configKey contextKey = "config"

var (
	verbose bool
	jsonOut bool
	rootCmd = &cobra.Command{
		Use:   "taskflow",
		Short: "Taskflow CLI",
		Long: `Taskflow connects ClickUp task management with local coding session tracking.
It maintains session history, progress checkpoints, and engineering snags.`,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if err := config.EnsureDir(); err != nil {
				return fmt.Errorf("failed to ensure config directory: %w", err)
			}
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}
			ctx := context.WithValue(cmd.Context(), configKey, cfg)
			cmd.SetContext(ctx)
			return nil
		},
	}
)

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "enable verbose logging")
	rootCmd.PersistentFlags().BoolVar(&jsonOut, "json", false, "output in JSON format")
}

func ConfigFromContext(ctx context.Context) *config.Config {
	cfg, ok := ctx.Value(configKey).(*config.Config)
	if !ok {
		return nil
	}
	return cfg
}

func JSONOutput() bool {
	return jsonOut
}
