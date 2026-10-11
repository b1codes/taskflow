package session

import (
	"context"
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
	// Match is the stable "[requirement] describe" part of Name, which finds
	// the item in ClickUp whatever its status text says (ChecklistItemMatches).
	Match string `json:"match"`
	// Name is Match plus its status: verify levels, attested, waived, or
	// struck through when void.
	Name     string `json:"name"`
	Resolved bool   `json:"resolved"`
	// Void items are only renamed, never created: they were dropped, and the
	// strike keeps an item already shown in the checklist's history.
	Void bool `json:"void,omitempty"`
}

// checklistItems is the checklist for obligations, in the order they were
// first seen.
func checklistItems(obligations []*Obligation) []ChecklistItem {
	items := make([]ChecklistItem, 0, len(obligations))
	for _, o := range obligations {
		items = append(items, checklistItem(o))
	}
	return items
}

func checklistItem(o *Obligation) ChecklistItem {
	match := "[" + o.RequirementID + "] " + o.Describe
	it := ChecklistItem{
		Key:   o.RuleID + "/" + o.RequirementID + "/" + o.Subject,
		Match: match,
		Name:  match,
		// Only open obligations hold up the definition of done; the name
		// says how the rest were resolved.
		Resolved: o.Status != ObligationOpen,
	}
	if o.Status == ObligationVoid {
		it.Name, it.Void = "~~"+match+"~~", true
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
		if l != levelAttest {
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

// ChecklistItemMatches reports whether a ClickUp checklist item named name is
// the item whose stable part is match: the name is match itself, match with
// a status suffix, or match struck through.
func ChecklistItemMatches(name, match string) bool {
	rest, ok := strings.CutPrefix(strings.TrimPrefix(name, "~~"), match)
	return ok && (rest == "" || strings.HasPrefix(rest, " — ") || strings.HasPrefix(rest, "~~"))
}

// enqueueChecklist queues the session's checklist as it stands in
// obligations.
func (s *Service) enqueueChecklist(ctx context.Context, sess *Session, obligations []*Obligation) {
	s.enqueueSync(ctx, sess.TaskID, OpSyncChecklist, ChecklistPayload{
		ChecklistName: ChecklistName,
		Items:         checklistItems(obligations),
	})
}
