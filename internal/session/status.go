package session

import "fmt"

type Status string

const (
	StatusActive    Status = "ACTIVE"
	StatusPaused    Status = "PAUSED"
	StatusCompleted Status = "COMPLETED"
	StatusArchived  Status = "ARCHIVED"
)

func (s Status) Valid() bool {
	switch s {
	case StatusActive, StatusPaused, StatusCompleted, StatusArchived:
		return true
	}
	return false
}

func ValidateTransition(from, to Status) error {
	if !from.Valid() {
		return fmt.Errorf("invalid current status: %s", from)
	}
	if !to.Valid() {
		return fmt.Errorf("invalid target status: %s", to)
	}
	if from == to {
		return nil
	}
	switch from {
	case StatusActive:
		return nil
	case StatusPaused:
		return nil
	case StatusCompleted:
		if to == StatusActive || to == StatusPaused {
			return fmt.Errorf("cannot transition from %s to %s", from, to)
		}
		return nil
	case StatusArchived:
		return fmt.Errorf("cannot transition from terminal status %s", from)
	}
	return fmt.Errorf("unsupported transition from %s to %s", from, to)
}

// StatusRole is a session lifecycle event that the sync worker maps onto one
// of the ClickUp list's statuses. Defined here (rather than in clickup) so
// session can enqueue roles without importing clickup; clickup aliases these.
type StatusRole string

const (
	// RoleStart moves a not-yet-started task into the list's working status.
	RoleStart StatusRole = "start"
	// RoleReopen is RoleStart for a task whose previous session was completed
	// or archived: it may also move the task out of a done/closed status.
	RoleReopen StatusRole = "reopen"
	// RoleComplete moves the task to the list's done (or closed) status.
	RoleComplete StatusRole = "complete"
	// RoleArchive moves an abandoned task to a cancel-like closed status.
	RoleArchive StatusRole = "archive"
)
