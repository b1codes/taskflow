package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/b1codes/taskflow/internal/drift"
	"github.com/b1codes/taskflow/internal/store"
	"github.com/spf13/cobra"
)

var (
	reportProject string
	reportSince   string
	reportFormat  string
)

var completenessReportCmd = &cobra.Command{
	Use:   "report",
	Short: "Drift report: where completeness rules aren't being followed, per b1 module",
	Long: `Aggregate obligations across sessions per (source, rule, requirement):
satisfied at first evaluation (AGENTS.md worked), satisfied after at least one
reminder (taskflow's reminders did the work), attested, waived (with similar
rationales clustered), and open at stop (the rule isn't being followed).

--format b1 prints markdown suggestions aimed at the b1 module files to change
(modules/<module>/context/ and completeness.yaml). Nothing is changed.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		format := reportFormat
		if JSONOutput() {
			format = "json"
		}
		switch format {
		case "text", "json", "b1":
		default:
			return fmt.Errorf("unknown --format %q: use text, json, or b1", reportFormat)
		}
		since, err := parseSince(reportSince, time.Now())
		if err != nil {
			return err
		}
		filter := store.DriftFilter{Since: since}
		if reportProject != "" {
			abs, err := filepath.Abs(reportProject)
			if err != nil {
				return err
			}
			filter.ProjectPath = abs
		}

		cfg := ConfigFromContext(cmd.Context())
		if cfg == nil {
			return fmt.Errorf("config not found in context")
		}
		st, err := store.New(cfg.DBPath())
		if err != nil {
			return fmt.Errorf("failed to open database: %w", err)
		}
		defer st.Close()

		rows, err := st.DriftReport(cmd.Context(), filter)
		if err != nil {
			return err
		}
		report := drift.Build(rows, filter)
		switch format {
		case "json":
			encoder := json.NewEncoder(os.Stdout)
			encoder.SetIndent("", "  ")
			if err := encoder.Encode(report); err != nil {
				return fmt.Errorf("failed to encode JSON: %w", err)
			}
		case "b1":
			fmt.Print(report.B1())
		default:
			fmt.Print(report.Text())
		}
		return nil
	},
}

// parseSince reads --since: a number of days ("30d"), weeks ("2w"), a Go
// duration ("12h"), or a date ("2026-09-01"), relative to now. "0" or ""
// means all time (the zero time).
func parseSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return time.Time{}, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t, nil
	}
	for suffix, unit := range map[string]time.Duration{"d": 24 * time.Hour, "w": 7 * 24 * time.Hour} {
		if n, ok := strings.CutSuffix(s, suffix); ok {
			if v, err := strconv.Atoi(n); err == nil && v >= 0 {
				return now.Add(-time.Duration(v) * unit), nil
			}
		}
	}
	if d, err := time.ParseDuration(s); err == nil && d >= 0 {
		return now.Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("invalid --since %q: use e.g. 30d, 2w, 12h, a date like 2026-09-01, or 0 for all time", s)
}

func init() {
	completenessReportCmd.Flags().StringVar(&reportProject, "project", "", "Only sessions in this project (default: every project)")
	completenessReportCmd.Flags().StringVar(&reportSince, "since", "30d", "Only obligations first seen in this window: 30d, 2w, 12h, a date, or 0 for all time")
	completenessReportCmd.Flags().StringVar(&reportFormat, "format", "text", "Output format: text, json, or b1 (suggestions for b1 modules)")

	completenessCmd.AddCommand(completenessReportCmd)
}
