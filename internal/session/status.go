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
