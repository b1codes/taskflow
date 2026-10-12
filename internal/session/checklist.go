package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// OpSyncChecklist is the sync operation that reconciles a task's Definition
// of Done checklist with its ChecklistPayload.
const OpSyncChecklist = "SYNC_CHECKLIST"

// ChecklistName is the ClickUp checklist taskflow keeps in sync.
const ChecklistName = "Taskflow · Definition of Done"

// ChecklistPayload is the full desired state of the checklist, so applying
// it is safe to retry and no ClickUp IDs need to be stored.
type ChecklistPayload struct {
	ChecklistName string          `json:"checklist_name"`
	Items         []ChecklistItem `json:"items"`
}

// ChecklistItem is one obligation as a checklist item.
type ChecklistItem struct {
	// Key is the obligation's rule/requirement/subject.
	Key string `json:"key"`
	// Match is the stable "[requirement] describe (subject)" part of Name,
	// which finds the item in ClickUp whatever its status text says
	// (ChecklistItemMatches). The subject keeps one requirement's items for
	// different files apart; items named before it was added ("[requirement]
	// describe") are found by the Match their Key had in earlier payloads. It
	// is normalized (NormalizeChecklistName), as ClickUp stores names.
	Match string `json:"match"`
	// Name is Match plus its status after " — ": verify levels, attested,
	// waived, or void.
	Name     string `json:"name"`
	Resolved bool   `json:"resolved"`
	// Void items are only renamed, never created: they were dropped, and the
	// "void" suffix keeps an item already shown in the checklist's history.
	Void bool `json:"void,omitempty"`
}

// checklistSubject is the " (subject)" part of an item's Match, so the same
// requirement on two files makes two items that say which file each is
// about. A rule with no stack has the subject "*", which names nothing.
func checklistSubject(subject string) string {
	if subject == "" || subject == "*" {
		return ""
	}
	return " (" + subject + ")"
}

// checklistVoid is the status suffix of a void item. ClickUp item names are
// plain text, so it is a word rather than a strikethrough.
const checklistVoid = "void"

// checklistItems is the checklist for obligations, in their order.
func checklistItems(obligations []*Obligation) []ChecklistItem {
	items := make([]ChecklistItem, 0, len(obligations))
	for _, o := range obligations {
		items = append(items, checklistItem(o))
	}
	return items
}

func checklistItem(o *Obligation) ChecklistItem {
	match := NormalizeChecklistName("[" + o.RequirementID + "] " + o.Describe + checklistSubject(o.Subject))
	it := ChecklistItem{
		Key:   o.RuleID + "/" + o.RequirementID + "/" + o.Subject,
		Match: match,
		Name:  match,
		// Only open obligations hold up the definition of done; the name
		// says how the rest were resolved.
		Resolved: o.Status != ObligationOpen,
	}
	if o.Status == ObligationVoid {
		it.Name, it.Void = match+" — "+checklistVoid, true
		return it
	}
	var parts []string
	switch {
	case o.Status == ObligationWaived:
		parts = append(parts, "waived")
	case o.attested():
		parts = append(parts, "attested")
	}
	var verified []string
	for _, l := range o.VerifiedLevels {
		// A level is one word in the name, so ChecklistItemMatches can tell
		// the suffix from the describe text.
		if l = strings.Join(strings.Fields(l), "-"); l != "" && l != levelAttest {
			verified = append(verified, l)
		}
	}
	if len(verified) > 0 {
		parts = append(parts, strings.Join(verified, ", ")+" ✓")
	}
	if len(parts) > 0 {
		it.Name += " — " + strings.Join(parts, ", ")
	}
	return it
}

// NormalizeChecklistName collapses runs of whitespace (newlines from a YAML
// block describe included) into single spaces and trims the ends, as ClickUp
// may do to an item name; names are built and compared normalized.
func NormalizeChecklistName(name string) string {
	return strings.Join(strings.Fields(name), " ")
}

// ChecklistItemMatches reports whether a ClickUp checklist item named name is
// the item whose stable part is match: the name is match itself, or match
// with a status suffix checklistItem builds, or match struck through (the
// void form of earlier builds). Any other text after match is part of a
// different item's describe ("[r] Add test — edge cases" isn't "[r] Add
// test"). Both are compared normalized.
func ChecklistItemMatches(name, match string) bool {
	name, match = NormalizeChecklistName(name), NormalizeChecklistName(match)
	if name == match || name == "~~"+match+"~~" {
		return true
	}
	suffix, ok := strings.CutPrefix(name, match+" — ")
	return ok && isChecklistSuffix(suffix)
}

// isChecklistSuffix reports whether s is a status suffix checklistItem
// builds: "void", or "waived"/"attested" and verify levels, in that order,
// joined by ", ", the levels as one-word names ending in " ✓".
func isChecklistSuffix(s string) bool {
	if s == checklistVoid {
		return true
	}
	if rest, ok := strings.CutPrefix(s, "waived"); ok {
		s = rest
	} else if rest, ok := strings.CutPrefix(s, "attested"); ok {
		s = rest
	} else {
		s = ", " + s // levels alone
	}
	if s == "" {
		return true
	}
	levels, ok := strings.CutSuffix(s, " ✓")
	if !ok {
		return false
	}
	levels, ok = strings.CutPrefix(levels, ", ")
	if !ok {
		return false
	}
	for _, l := range strings.Split(levels, ", ") {
		switch {
		case l == "" || strings.ContainsAny(l, " ,"):
			return false
		case l == checklistVoid || l == "waived" || l == "attested":
			return false // a status, out of place
		}
	}
	return true
}

// enqueueChecklist queues the session's checklist as it stands in
// obligations unless ClickUp has it or will: it is queued when the task's
// latest SYNC_CHECKLIST op carries a different checklist or failed for good,
// or when there is no op yet. Comparing with what was queued, rather than
// with the last merge, also repairs a checklist that drifted (its op ran out
// of retries, or the session predates the checklist). When the latest op
// can't be read, the checklist is queued: a redundant op changes nothing in
// ClickUp, and a missing one would leave it out of date. An empty checklist
// is never queued: a session's obligations are never deleted, so there is
// nothing to clear.
func (s *Service) enqueueChecklist(ctx context.Context, sess *Session, obligations []*Obligation) {
	if len(obligations) == 0 {
		return
	}
	payload := ChecklistPayload{ChecklistName: ChecklistName, Items: checklistItems(obligations)}
	latest, err := s.store.LatestSyncOp(ctx, sess.TaskID, OpSyncChecklist)
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "[session] failed to read the latest checklist op for task %s: %v\n", sess.TaskID, err)
	case latest != nil && !(latest.Status == "FAILED" && latest.Retries >= latest.MaxRetries):
		var queued ChecklistPayload
		if json.Unmarshal([]byte(latest.Payload), &queued) == nil && sameChecklist(queued, payload) {
			return
		}
	}
	s.enqueueSync(ctx, sess.TaskID, OpSyncChecklist, payload)
}

// sameChecklist reports whether a and b carry the same items, whatever their
// order: obligations merged in memory come in finding order, and read back
// from the store in the order they were first seen, which can differ for the
// same checklist. The worker finds each item by its name, so the order only
// decides where items it has to create go.
func sameChecklist(a, b ChecklistPayload) bool {
	if a.ChecklistName != b.ChecklistName || len(a.Items) != len(b.Items) {
		return false
	}
	byKey := make(map[string]ChecklistItem, len(a.Items))
	for _, it := range a.Items {
		byKey[it.Key] = it
	}
	for _, it := range b.Items {
		if have, ok := byKey[it.Key]; !ok || have != it {
			return false
		}
		delete(byKey, it.Key) // each item pairs once, so a repeated key differs
	}
	return len(byKey) == 0
}
