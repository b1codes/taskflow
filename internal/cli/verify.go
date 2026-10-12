package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/b1codes/taskflow/internal/store"
	"github.com/spf13/cobra"
)

var verifyCmd = &cobra.Command{
	Use:   "verify [session_id]",
	Short: "Run the session's completeness command checks now (test suites, Storybook test-runner)",
	Long: `Run the session's command checks now rather than at stop: each obligation
whose requirement names a command (run: in the completeness manifest, e.g. the
stack's test suite) runs it in the stack's root, and passes on exit status 0.
Commands come only from the project's manifest and overrides.

Needs allow_commands = true under [completeness] in ~/.taskflow/config.toml;
each command is limited to command_timeout_s seconds (default 300).`,
	Args: cobra.ExactArgs(1),
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

		report, err := newSessionService(st, cfg).Verify(cmd.Context(), args[0])
		if err != nil {
			return err
		}

		if JSONOutput() {
			encoder := json.NewEncoder(os.Stdout)
			encoder.SetIndent("", "  ")
			if err := encoder.Encode(report); err != nil {
				return fmt.Errorf("failed to encode JSON: %w", err)
			}
			return nil
		}
		printReport(report)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(verifyCmd)
}
