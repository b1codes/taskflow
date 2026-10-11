package cli

import (
	"testing"

	"github.com/b1codes/taskflow/internal/config"
	"github.com/b1codes/taskflow/internal/session"
)

// TestCLIGate: the CLI parses the configured gate, and one that got past
// config.Load fails closed as block instead of acting as report.
func TestCLIGate(t *testing.T) {
	for gate, want := range map[string]session.GateMode{
		"":       session.GateReview,
		"report": session.GateReport,
		"block":  session.GateBlock,
		"blok":   session.GateBlock,
	} {
		if got := cliGate(&config.Config{Completeness: config.CompletenessConfig{Gate: gate}}); got != want {
			t.Errorf("cliGate(%q) = %q, want %q", gate, got, want)
		}
	}
}
