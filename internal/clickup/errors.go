package clickup

import (
	"errors"
	"fmt"
	"time"
)

var (
	ErrUnauthorized     = errors.New("ClickUp API key is invalid or expired. Set CLICKUP_API_KEY and try again.")
	ErrRateLimited      = errors.New("ClickUp API rate limit exceeded")
	ErrNotFound         = errors.New("ClickUp resource not found")
	ErrServerError      = errors.New("ClickUp server error")
	ErrNoMatchingStatus = errors.New("no matching status found on list")
	// ErrBadRequest is a 400 response, e.g. a status name the list no longer has.
	ErrBadRequest = errors.New("ClickUp rejected the request")
	// ErrStatusRejected wraps the ErrBadRequest of a status update that
	// ApplyStatusRole couldn't fix by refreshing the list's statuses, so a
	// retry would be rejected the same way.
	ErrStatusRejected = errors.New("ClickUp rejected the status update")
)

type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("ClickUp API rate limit exceeded; retry after %v", e.RetryAfter)
	}
	return "ClickUp API rate limit exceeded"
}

func (e *RateLimitError) Is(target error) bool {
	return target == ErrRateLimited
}
