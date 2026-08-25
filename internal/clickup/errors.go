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
	ErrNoMatchingStatus = errors.New("no matching status found for target status type")
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
