// Package drift turns the store's per-requirement obligation counts into the
// completeness drift report: where the rules b1 modules ship aren't being
// followed, and what to change in which module. It suggests; it never edits
// anything.
package drift

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/b1codes/taskflow/internal/store"
)

// MinSample is how many decided obligations a requirement needs before the
// report suggests anything about it: fewer is anecdote.
const MinSample = 3

// Thresholds above which a requirement gets a suggestion, as a share of its
// decided obligations (or, for reminders, of its satisfied ones).
const (
	openAtStopShare = 0.25
	waivedShare     = 0.30
	remindedShare   = 0.50
)

// Signal is what a requirement's numbers say about its rule.
type Signal string

const (
	// SignalUnfollowed: often still open when the session stopped.
	SignalUnfollowed Signal = "unfollowed"
	// SignalOftenWaived: often waived, so the rule may be too broad.
	SignalOftenWaived Signal = "often-waived"
	// SignalReminderDependent: done mostly after taskflow's reminders, so
	// the AGENTS.md guidance isn't doing it.
	SignalReminderDependent Signal = "reminder-dependent"
)

// Report is the drift report.
type Report struct {
	Since        *time.Time    `json:"since,omitempty"`
	Project      string        `json:"project,omitempty"`
	Requirements []Requirement `json:"requirements"`
}

// Requirement is one (source, rule, requirement) and how its obligations
// ended up.
type Requirement struct {
	store.DriftRow
	// Decided are the obligations that ended one way or another (all but
	// those open in sessions still in progress).
	Decided int `json:"decided"`
	// WaiverClusters group similar waiver rationales, largest first.
	WaiverClusters []Cluster `json:"waiver_clusters,omitempty"`
	Signals        []Signal  `json:"signals,omitempty"`
}

// Cluster is a group of similar waiver rationales.
type Cluster struct {
	Count int `json:"count"`
	// Terms are the words every rationale in the cluster shares.
	Terms []string `json:"terms,omitempty"`
	// Example is the cluster's first rationale.
	Example string `json:"example"`
}

// Build makes the report from the store's rows.
func Build(rows []*store.DriftRow, filter store.DriftFilter) *Report {
	r := &Report{Project: filter.ProjectPath, Requirements: []Requirement{}}
	if !filter.Since.IsZero() {
		since := filter.Since
		r.Since = &since
	}
	for _, row := range rows {
		q := Requirement{DriftRow: *row}
		q.WaiverRationales = nil // clustered below; the raw list is long
		q.Decided = row.SatisfiedFirst + row.SatisfiedAfterReminder + row.Attested + row.Waived + row.OpenAtStop
		q.WaiverClusters = ClusterRationales(row.WaiverRationales)
		q.Signals = signals(&q)
		r.Requirements = append(r.Requirements, q)
	}
	return r
}

func signals(q *Requirement) []Signal {
	if q.Decided < MinSample {
		return nil
	}
	var out []Signal
	share := func(n, of int) float64 { return float64(n) / float64(of) }
	if share(q.OpenAtStop, q.Decided) >= openAtStopShare {
		out = append(out, SignalUnfollowed)
	}
	if share(q.Waived, q.Decided) >= waivedShare {
		out = append(out, SignalOftenWaived)
	}
	if sat := q.SatisfiedFirst + q.SatisfiedAfterReminder; sat >= MinSample && share(q.SatisfiedAfterReminder, sat) >= remindedShare {
		out = append(out, SignalReminderDependent)
	}
	return out
}

// clusterSimilarity is the Jaccard similarity of significant words at which
// two rationales count as saying the same thing.
const clusterSimilarity = 0.5

// ClusterRationales groups rationales that share most of their significant
// words, greedily against each cluster's first rationale, largest cluster
// first.
func ClusterRationales(rationales []string) []Cluster {
	type group struct {
		first  map[string]bool
		shared map[string]bool
		c      Cluster
	}
	var groups []*group
	for _, text := range rationales {
		ws := wordSet(text)
		var best *group
		bestSim := 0.0
		for _, g := range groups {
			if sim := jaccard(ws, g.first); sim >= clusterSimilarity && sim > bestSim {
				best, bestSim = g, sim
			}
		}
		if best == nil {
			shared := map[string]bool{}
			for w := range ws {
				shared[w] = true
			}
			groups = append(groups, &group{first: ws, shared: shared, c: Cluster{Count: 1, Example: oneLine(text)}})
			continue
		}
		best.c.Count++
		for w := range best.shared {
			if !ws[w] {
				delete(best.shared, w)
			}
		}
	}
	out := make([]Cluster, 0, len(groups))
	for _, g := range groups {
		if g.c.Count > 1 {
			for w := range g.shared {
				g.c.Terms = append(g.c.Terms, w)
			}
			sort.Strings(g.c.Terms)
		}
		out = append(out, g.c)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out
}

var stopwords = map[string]bool{
	"the": true, "and": true, "for": true, "not": true, "are": true, "this": true, "that": true, "with": true,
	"has": true, "have": true, "was": true, "but": true, "its": true, "it's": true, "all": true, "any": true,
	"from": true, "into": true, "only": true, "does": true, "doesn't": true, "don't": true, "isn't": true,
	"there": true, "here": true, "will": true, "would": true, "should": true, "can": true, "cannot": true,
	"because": true, "already": true, "yet": true, "our": true, "their": true, "which": true, "when": true,
	"apply": true, "applies": true, "needed": true, "need": true, "needs": true,
}

// wordSet is text's significant words: lowercased, three letters or more,
// stopwords dropped.
func wordSet(text string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\''
	}) {
		w = strings.Trim(w, "'")
		if len([]rune(w)) >= 3 && !stopwords[w] {
			out[w] = true
		}
	}
	return out
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	inter := 0
	for w := range a {
		if b[w] {
			inter++
		}
	}
	return float64(inter) / float64(len(a)+len(b)-inter)
}

// oneLine collapses whitespace and shortens text for a report line.
func oneLine(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if r := []rune(text); len(r) > 120 {
		text = string(r[:119]) + "…"
	}
	return text
}

// Module is the b1 module a source names ("testing/playwright@1.0.0" →
// "testing/playwright"), or "" for a rule no module contributed.
func Module(source string) string {
	module, _, _ := strings.Cut(source, "@")
	return strings.TrimSpace(module)
}

// suggestion is one change the b1 format proposes.
type suggestion struct {
	target string // the file or directory to change
	text   string
}

// suggestions are q's proposed changes, aimed at its module's files in the
// b1 library (modules/<module>/), or at the project's overrides when no
// module contributed the rule.
func (q *Requirement) suggestions() []suggestion {
	module := Module(q.Source)
	context, rules := ".agents/project/AGENTS.md", ".agents/project/completeness.yaml"
	if module != "" {
		context = "modules/" + module + "/context/"
		rules = "modules/" + module + "/completeness.yaml"
	}
	id := "`" + q.RuleID + "/" + q.RequirementID + "`"
	var out []suggestion
	for _, s := range q.Signals {
		switch s {
		case SignalUnfollowed:
			out = append(out, suggestion{context, fmt.Sprintf(
				"%s was still open at stop in %d of %d — the guidance isn't being followed: add a concrete example of the expected artifact, and consider `gate = \"block\"` or the `taskflow check --hook claude` Stop hook for it.",
				id, q.OpenAtStop, q.Decided)})
		case SignalOftenWaived:
			text := fmt.Sprintf("%s was waived in %d of %d — the rule may be too broad: narrow its `when`/`exclude`, add an `only_if`, or lower its severity to `recommended`.",
				id, q.Waived, q.Decided)
			if len(q.WaiverClusters) > 0 {
				var parts []string
				for _, c := range q.WaiverClusters[:min(3, len(q.WaiverClusters))] {
					parts = append(parts, fmt.Sprintf("%d× %q", c.Count, c.Example))
				}
				text += " Common rationales: " + strings.Join(parts, "; ") + "."
			}
			out = append(out, suggestion{rules, text})
		case SignalReminderDependent:
			sat := q.SatisfiedFirst + q.SatisfiedAfterReminder
			out = append(out, suggestion{context, fmt.Sprintf(
				"%s was satisfied only after reminders in %d of %d — the AGENTS.md guidance is weak: state the requirement where the work is described, with an example.",
				id, q.SatisfiedAfterReminder, sat)})
		}
	}
	return out
}

// B1 renders markdown suggestions per b1 module for the module context files
// and rules that need attention. Nothing is changed automatically.
func (r *Report) B1() string {
	type entry struct {
		target string
		texts  []string
	}
	byModule := map[string][]*entry{}
	var modules []string
	for i := range r.Requirements {
		q := &r.Requirements[i]
		module := Module(q.Source)
		for _, s := range q.suggestions() {
			if _, ok := byModule[module]; !ok {
				modules = append(modules, module)
			}
			entries := byModule[module]
			idx := slices.IndexFunc(entries, func(e *entry) bool { return e.target == s.target })
			if idx < 0 {
				entries = append(entries, &entry{target: s.target})
				idx = len(entries) - 1
			}
			entries[idx].texts = append(entries[idx].texts, s.text)
			byModule[module] = entries
		}
	}
	var b strings.Builder
	b.WriteString("# Completeness drift: suggestions for b1 modules\n\n")
	b.WriteString(r.scope())
	b.WriteString(" Taskflow found where the rules aren't being followed; update the modules with `b1`, then re-run `b1 pair`. Nothing was changed.\n")
	if len(modules) == 0 {
		fmt.Fprintf(&b, "\nNo suggestions: no requirement with at least %d decided obligations crossed a threshold.\n", MinSample)
		return b.String()
	}
	sort.Slice(modules, func(i, j int) bool {
		// Project rules last.
		if (modules[i] == "") != (modules[j] == "") {
			return modules[j] == ""
		}
		return modules[i] < modules[j]
	})
	for _, module := range modules {
		if module == "" {
			b.WriteString("\n## Project rules (no b1 module)\n")
		} else {
			fmt.Fprintf(&b, "\n## `%s`\n", module)
		}
		for _, e := range byModule[module] {
			fmt.Fprintf(&b, "\n**`%s`**\n", e.target)
			for _, t := range e.texts {
				fmt.Fprintf(&b, "- %s\n", t)
			}
		}
	}
	return b.String()
}

// scope says what the report covers.
func (r *Report) scope() string {
	project := "all projects"
	if r.Project != "" {
		project = "`" + r.Project + "`"
	}
	if r.Since == nil {
		return fmt.Sprintf("Obligations in %s, all time.", project)
	}
	return fmt.Sprintf("Obligations in %s first seen since %s.", project, r.Since.Format("2006-01-02"))
}
