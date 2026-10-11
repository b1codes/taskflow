package session

import (
	"context"
	"errors"
	"testing"
)

// TestGateMode_FailsClosed: an unknown gate ranks as block, so a value that
// slipped past validation can only tighten the gate.
func TestGateMode_FailsClosed(t *testing.T) {
	tests := []struct {
		g, other, want GateMode
	}{
		{"blok", GateReport, GateBlock},
		{GateReport, "blok", GateBlock},
		{"blok", GateReview, GateBlock},
		{"", GateReview, GateReview}, // a manifest without a gate defers to config
		{GateReview, "", GateReview},
		{"", "", ""},
		{"", "blok", GateBlock},
		{GateReport, GateReview, GateReview},
		{GateBlock, GateReview, GateBlock},
		{GateReport, GateReport, GateReport},
	}
	for _, tt := range tests {
		if got := tt.g.stricter(tt.other); got != tt.want {
			t.Errorf("GateMode(%q).stricter(%q) = %q, want %q", tt.g, tt.other, got, tt.want)
		}
	}

	if g, err := ParseGateMode("blok"); err == nil || g != "" {
		t.Errorf("ParseGateMode(blok) = %q, %v; want an error", g, err)
	}
	if g, err := ParseGateMode(""); err != nil || g != DefaultGate {
		t.Errorf("ParseGateMode(\"\") = %q, %v; want the default gate", g, err)
	}
	for _, g := range []GateMode{GateReport, GateReview, GateBlock} {
		if got, err := ParseGateMode(string(g)); err != nil || got != g {
			t.Errorf("ParseGateMode(%q) = %q, %v", g, got, err)
		}
	}
}

// TestService_UnknownConfiguredGateBlocks: a configured gate that bypassed
// ParseGateMode blocks completion instead of acting as report.
func TestService_UnknownConfiguredGateBlocks(t *testing.T) {
	ctx := context.Background()

	t.Run("open required work", func(t *testing.T) {
		srv, store, dir := newCompletenessService(t, &fakeEvaluator{eval: screenEval()})
		srv.WithGate("blok")
		res, err := srv.Start(ctx, "task-1", dir, false)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if g := res.AgenticContract.Completeness.Gate; g != GateBlock {
			t.Errorf("contract gate = %q, want block", g)
		}
		sid := res.Session.ID
		if _, err := srv.Stop(ctx, sid, "", "COMPLETED", false, false); !errors.Is(err, ErrIncompleteSession) {
			t.Fatalf("Stop = %v; want ErrIncompleteSession", err)
		}
		if store.sessions[sid].Status != StatusActive {
			t.Errorf("status = %s; a blocked stop must change nothing", store.sessions[sid].Status)
		}
	})

	t.Run("invalid manifest", func(t *testing.T) {
		ev := screenEval()
		ev.Error = "rules[0].id: required"
		srv, _, dir := newCompletenessService(t, &fakeEvaluator{eval: ev})
		srv.WithGate("blok")
		res, err := srv.Start(ctx, "task-1", dir, false)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if _, err := srv.Stop(ctx, res.Session.ID, "", "COMPLETED", false, false); !errors.Is(err, ErrCompletenessUnchecked) {
			t.Errorf("Stop = %v; want ErrCompletenessUnchecked", err)
		}
	})
}
