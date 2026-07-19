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
	snagError      string
	snagCategory   string
	snagResolution string
)

var snagCmd = &cobra.Command{
	Use:   "snag [session_id]",
	Short: "Log an engineering snag",
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

		res, err := srv.LogSnag(cmd.Context(), sessionID, snagError, snagCategory, snagResolution)
		if err != nil {
			return err
		}

		if JSONOutput() {
			encoder := json.NewEncoder(os.Stdout)
			encoder.SetIndent("", "  ")
			if err := encoder.Encode(res); err != nil {
				return fmt.Errorf("failed to encode JSON: %w", err)
			}
		} else {
			fmt.Printf("✓ Snag logged: %s\n", res.Snag.ID)
			if len(res.RelatedResolutions) > 0 {
				fmt.Println("\n💡 Found matching resolutions from past projects:")
				for _, r := range res.RelatedResolutions {
					fmt.Printf("  - Project:    %s\n", r.SourceProject)
					fmt.Printf("    Signature:  %s\n", r.ErrorSignature)
					fmt.Printf("    Resolution: %s\n", r.Resolution)
				}
			}
		}

		return nil
	},
}

func init() {
	snagCmd.Flags().StringVarP(&snagError, "error", "e", "", "The full error message text")
	snagCmd.Flags().StringVarP(&snagCategory, "category", "c", "build", "The category of the error (e.g. build, runtime, test)")
	snagCmd.Flags().StringVarP(&snagResolution, "resolution", "r", "", "The resolution if it is already solved")
	_ = snagCmd.MarkFlagRequired("error")

	rootCmd.AddCommand(snagCmd)
}
