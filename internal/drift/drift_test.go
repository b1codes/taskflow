package drift

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/b1codes/taskflow/internal/store"
)

func TestClusterRationales(t *testing.T) {
	got := ClusterRationales([]string{
		"Screen is behind a feature flag, no route yet",
		"Feature flag screen: no route yet",
		"Pure refactor, behaviour unchanged",
		"behind a feature flag - no route",
	})
	if len(got) != 2 {
		t.Fatalf("clusters = %+v", got)
	}
	if got[0].Count != 3 || got[0].Example != "Screen is behind a feature flag, no route yet" ||
		!slices.Equal(got[0].Terms, []string{"feature", "flag", "route"}) {
		t.Errorf("largest cluster = %+v", got[0])
	}
	if got[1].Count != 1 || got[1].Terms != nil {
		t.Errorf("singleton = %+v", got[1])
	}
	if len(ClusterRationales(nil)) != 0 {
		t.Error("no rationales, no clusters")
	}
}

func TestModule(t *testing.T) {
	for in, want := range map[string]string{"testing/playwright@1.0.0": "testing/playwright", "framework/react-web": "framework/react-web", "": ""} {
		if got := Module(in); got != want {
			t.Errorf("Module(%q) = %q, want %q", in, got, want)
		}
	}
}

func rows() []*store.DriftRow {
	return []*store.DriftRow{
		// Followed: no signal.
		{Source: "testing/storybook", RuleID: "react-screen", RequirementID: "story", Obligations: 10, Sessions: 10, SatisfiedFirst: 9, SatisfiedAfterReminder: 1},
		// Done, but only after reminders.
		{Source: "testing/playwright@1.0.0", RuleID: "react-screen", RequirementID: "e2e", Obligations: 10, Sessions: 10, SatisfiedFirst: 3, SatisfiedAfterReminder: 7},
		// Often open at stop and often waived, in a project rule.
		{RuleID: "local", RequirementID: "contract", Obligations: 6, Sessions: 5, OpenAtStop: 2, Waived: 3, SatisfiedFirst: 1,
			WaiverRationales: []string{"no services changed", "No services were changed", "docs only"}},
		// Too few to judge.
		{Source: "testing/contract", RuleID: "api", RequirementID: "contract", Obligations: 2, Sessions: 2, OpenAtStop: 2},
	}
}

func TestBuild_Signals(t *testing.T) {
	r := Build(rows(), store.DriftFilter{})
	got := map[string][]Signal{}
	for _, q := range r.Requirements {
		got[q.RuleID+"/"+q.RequirementID] = q.Signals
		if q.WaiverRationales != nil {
			t.Errorf("%s: raw rationales should be clustered, not listed", q.RequirementID)
		}
	}
	want := map[string][]Signal{
		"react-screen/story": nil,
		"react-screen/e2e":   {SignalReminderDependent},
		"local/contract":     {SignalUnfollowed, SignalOftenWaived},
		"api/contract":       nil,
	}
	for k, w := range want {
		if !slices.Equal(got[k], w) {
			t.Errorf("%s signals = %v, want %v", k, got[k], w)
		}
	}
	if c := r.Requirements[2].WaiverClusters; len(c) != 2 || c[0].Count != 2 {
		t.Errorf("clusters = %+v", c)
	}
}

func TestReport_B1(t *testing.T) {
	r := Build(rows(), store.DriftFilter{ProjectPath: "/p", Since: time.Date(2026, 9, 12, 0, 0, 0, 0, time.Local)})
	out := r.B1()
	for _, want := range []string{
		"Obligations in `/p` first seen since 2026-09-12.",
		"## `testing/playwright`",
		"**`modules/testing/playwright/context/`**",
		"`react-screen/e2e` was satisfied only after reminders in 7 of 10",
		"## Project rules (no b1 module)",
		"**`.agents/project/AGENTS.md`**",
		"`local/contract` was still open at stop in 2 of 6",
		"**`.agents/project/completeness.yaml`**",
		`Common rationales: 2× "no services changed"; 1× "docs only".`,
		"Nothing was changed.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("b1 output missing %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"testing/storybook", "testing/contract"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("b1 output shouldn't mention %s (followed, or too few):\n%s", unwanted, out)
		}
	}
	if strings.Index(out, "testing/playwright") > strings.Index(out, "Project rules") {
		t.Error("project rules should come after the modules")
	}

	empty := Build(nil, store.DriftFilter{}).B1()
	if !strings.Contains(empty, "No suggestions") || !strings.Contains(empty, "all projects, all time") {
		t.Errorf("empty b1 = %s", empty)
	}
}

func TestReport_Text(t *testing.T) {
	out := Build(rows(), store.DriftFilter{}).Text()
	for _, want := range []string{"RULE/REQUIREMENT", "testing/playwright@1.0.0", "(project)", "! local/contract: unfollowed",
		`– local/contract waived 2×: "no services changed" (changed, services)`} {
		if !strings.Contains(out, want) {
			t.Errorf("text missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(Build(nil, store.DriftFilter{}).Text(), "No obligations recorded.") {
		t.Error("empty text report")
	}
}
