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
	decisionTopic     string
	decisionText      string
	decisionRationale string
	decisionStatus    string
	decisionFiles     []string
)

var decisionCmd = &cobra.Command{
	Use:   "decision [session_id]",
	Short: "Log a design decision or dialogue rationale",
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

		res, err := srv.LogDecision(cmd.Context(), sessionID, decisionTopic, decisionText, decisionRationale, decisionStatus, decisionFiles)
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
			fmt.Printf("✓ Decision logged: %s (Topic: %s)\n", res.ID, res.Topic)
		}

		return nil
	},
}

func init() {
	decisionCmd.Flags().StringVarP(&decisionTopic, "topic", "t", "", "Topic or subject of the decision")
	decisionCmd.Flags().StringVarP(&decisionText, "decision", "d", "", "The agreed decision")
	decisionCmd.Flags().StringVarP(&decisionRationale, "rationale", "r", "", "Detailed explanation or context")
	decisionCmd.Flags().StringVarP(&decisionStatus, "status", "s", "no_change", "Outcome status (no_change, accepted, rejected_alternative, postponed)")
	decisionCmd.Flags().StringSliceVarP(&decisionFiles, "files", "f", nil, "Optional comma-separated list of related file paths")
	_ = decisionCmd.MarkFlagRequired("topic")
	_ = decisionCmd.MarkFlagRequired("decision")
	_ = decisionCmd.MarkFlagRequired("rationale")

	rootCmd.AddCommand(decisionCmd)
}
