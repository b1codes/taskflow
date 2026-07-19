package cli

import (
	"fmt"

	"github.com/b1codes/taskflow/internal/store"
	"github.com/spf13/cobra"
)

var refreshFlag bool

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize database and configuration",
	Long:  `Initializes the local SQLite database and caches the ClickUp workspace topology.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := ConfigFromContext(cmd.Context())
		if cfg == nil {
			return fmt.Errorf("config not found in context")
		}

		dbPath := cfg.DBPath()
		st, err := store.New(dbPath)
		if err != nil {
			return fmt.Errorf("failed to initialize database: %w", err)
		}
		defer st.Close()

		version, tableCount, err := st.GetSchemaInfo(cmd.Context())
		if err != nil {
			return fmt.Errorf("failed to retrieve database schema info: %w", err)
		}

		fmt.Printf("✓ Database initialized at %s\n", dbPath)
		fmt.Printf("Schema version: %d, Table count: %d\n", version, tableCount)

		// ClickUp workspace scan will be added in Phase 4.
		return nil
	},
}

func init() {
	initCmd.Flags().BoolVar(&refreshFlag, "refresh", false, "force a full refresh of cached ClickUp topology")
	rootCmd.AddCommand(initCmd)
}
