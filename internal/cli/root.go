package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/b1codes/taskflow/internal/adapters"
	"github.com/b1codes/taskflow/internal/clickup"
	"github.com/b1codes/taskflow/internal/config"
	"github.com/b1codes/taskflow/internal/session"
	"github.com/b1codes/taskflow/internal/store"
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
		var exit *ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.Code)
		}
		os.Exit(1)
	}
}

// ExitError ends the command with exit status Code and no error message: the
// command has printed what it has to say (e.g. check's open obligations).
type ExitError struct {
	Code int
}

func (e *ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// exitWith returns an *ExitError for cmd, silencing cobra's error and usage
// output for it.
func exitWith(cmd *cobra.Command, code int) error {
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	return &ExitError{Code: code}
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

func newSessionService(st store.Store, cfg *config.Config) *session.Service {
	var cuClient session.ClickUpClient
	if cfg != nil && cfg.ClickUp.APIKey != "" {
		cuClient = adapters.NewClickUpAdapter(clickup.New(cfg.ClickUp.APIKey))
	}
	evaluator := adapters.NewCompletenessEvaluator()
	if cfg != nil {
		evaluator.WithCommands(cfg.Completeness.AllowCommands, cfg.Completeness.CommandTimeout())
	}
	srv := session.NewService(st, adapters.NewGitCapturer(), cuClient).
		WithCompleteness(evaluator)
	if cfg != nil {
		srv.WithGate(cliGate(cfg))
	}
	return srv
}

// cliGate is the configured completion gate. PersistentPreRunE has already
// rejected an invalid gate (config.Load validates it, failing the command
// before it runs), so the error branch only guards a Config built some other
// way: it fails closed as block rather than letting a bad value loosen the
// gate.
func cliGate(cfg *config.Config) session.GateMode {
	g, err := cfg.Completeness.GateMode()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: %v; using the block gate\n", err)
		return session.GateBlock
	}
	return g
}
