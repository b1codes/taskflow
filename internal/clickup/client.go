package clickup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

type Client struct {
	httpClient *http.Client
	apiKey     string
	baseURL    string

	// minInterval is the minimum gap between the starts of consecutive
	// requests; zero disables pacing. nextSlot is the earliest time the next
	// request may start, guarded by paceMu since one client can be shared
	// across goroutines (e.g. the MCP server and the sync worker).
	minInterval time.Duration
	paceMu      sync.Mutex
	nextSlot    time.Time
}

func New(apiKey string) *Client {
	baseURL := "https://api.clickup.com/api/v2"
	if envURL := os.Getenv("CLICKUP_BASE_URL"); envURL != "" {
		baseURL = envURL
	}
	return &Client{
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		apiKey:  apiKey,
		baseURL: baseURL,
	}
}

// WithBaseURL allows overriding the base URL (useful for tests)
func (c *Client) WithBaseURL(url string) *Client {
	c.baseURL = url
	return c
}

// WithMinInterval paces requests so each one starts at least d after the
// previous one started, across all goroutines sharing this client. Zero (the
// default) disables pacing.
func (c *Client) WithMinInterval(d time.Duration) *Client {
	c.minInterval = d
	return c
}

// waitForSlot reserves the next request slot and blocks until it arrives or
// ctx is done. The slot is reserved under the lock and the sleep happens
// outside it, so concurrent callers queue up one interval apart.
func (c *Client) waitForSlot(ctx context.Context) error {
	if c.minInterval <= 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	c.paceMu.Lock()
	now := time.Now()
	slot := c.nextSlot
	if slot.Before(now) {
		slot = now
	}
	c.nextSlot = slot.Add(c.minInterval)
	c.paceMu.Unlock()

	delay := time.Until(slot)
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *Client) do(ctx context.Context, method, path string, bodyVal interface{}, result interface{}) error {
	var body io.Reader
	if bodyVal != nil {
		b, err := json.Marshal(bodyVal)
		if err != nil {
			return fmt.Errorf("failed to marshal request body: %w", err)
		}
		body = bytes.NewReader(b)
	}

	url := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return fmt.Errorf("failed to create http request: %w", err)
	}

	req.Header.Set("Authorization", c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	if err := c.waitForSlot(ctx); err != nil {
		return err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http request execution failed: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusAccepted, http.StatusNoContent:
		if result != nil {
			dec := json.NewDecoder(resp.Body)
			if err := dec.Decode(result); err != nil {
				return fmt.Errorf("failed to decode response: %w", err)
			}
		}
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrUnauthorized
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusBadRequest:
		respBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%w: %s", ErrBadRequest, string(respBytes))
	case http.StatusTooManyRequests:
		retryAfterStr := resp.Header.Get("Retry-After")
		var delay time.Duration
		if retryAfterStr != "" {
			if secs, err := strconv.Atoi(retryAfterStr); err == nil {
				delay = time.Duration(secs) * time.Second
			} else if t, err := http.ParseTime(retryAfterStr); err == nil {
				delay = time.Until(t)
			}
		}
		if delay < 0 {
			delay = 0
		}
		return &RateLimitError{RetryAfter: delay}
	default:
		if resp.StatusCode >= 500 {
			return ErrServerError
		}
		// Read error message from body if available
		respBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unexpected status code %d: %s", resp.StatusCode, string(respBytes))
	}
}
