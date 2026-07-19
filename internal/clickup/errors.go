package clickup

import "errors"

var (
	ErrUnauthorized = errors.New("ClickUp API key is invalid or expired. Set CLICKUP_API_KEY and try again.")
	ErrRateLimited  = errors.New("ClickUp API rate limit exceeded")
	ErrNotFound     = errors.New("ClickUp resource not found")
	ErrServerError  = errors.New("ClickUp server error")
)
