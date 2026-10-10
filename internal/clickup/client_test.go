package clickup

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestClickUpClient_GetTask(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "my-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "abc123",
			"name": "Implement user auth",
			"text_content": "Description info",
			"status": {
				"status": "in progress",
				"type": "active"
			},
			"list": {
				"id": "list-1",
				"name": "Task List"
			}
		}`))
	}))
	defer server.Close()

	client := New("my-key").WithBaseURL(server.URL)
	task, err := client.GetTask(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("failed to get task: %v", err)
	}

	if task.ID != "abc123" || task.Name != "Implement user auth" || task.Status.Type != "active" {
		t.Errorf("task fields mismatch: %+v", task)
	}
}

func TestClickUpClient_Errors(t *testing.T) {
	tests := []struct {
		status   int
		expected error
	}{
		{http.StatusUnauthorized, ErrUnauthorized},
		{http.StatusTooManyRequests, ErrRateLimited},
		{http.StatusNotFound, ErrNotFound},
		{http.StatusInternalServerError, ErrServerError},
	}

	for _, tt := range tests {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tt.status)
		}))

		client := New("my-key").WithBaseURL(server.URL)
		_, err := client.GetTask(context.Background(), "abc123")
		if !errors.Is(err, tt.expected) {
			t.Errorf("expected error %v for status %d, got %v", tt.expected, tt.status, err)
		}
		server.Close()
	}
}

func TestClickUpClient_WithMinInterval_PacesRequests(t *testing.T) {
	var mu sync.Mutex
	var starts []time.Time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		starts = append(starts, time.Now())
		mu.Unlock()
		_, _ = w.Write([]byte(`{"id": "abc123"}`))
	}))
	defer server.Close()

	const interval = 50 * time.Millisecond
	client := New("my-key").WithBaseURL(server.URL).WithMinInterval(interval)

	// Requests from several goroutines share the same pacing.
	begin := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := client.GetTask(context.Background(), "abc123"); err != nil {
				t.Errorf("GetTask failed: %v", err)
			}
		}()
	}
	wg.Wait()

	if elapsed := time.Since(begin); elapsed < 2*interval {
		t.Errorf("expected 3 paced requests to take at least %v, took %v", 2*interval, elapsed)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(starts) != 3 {
		t.Fatalf("expected 3 requests, got %d", len(starts))
	}
	for i := 1; i < len(starts); i++ {
		// Allow a little slack for the time between leaving the pacer and the
		// server recording the request.
		if gap := starts[i].Sub(starts[i-1]); gap < interval-10*time.Millisecond {
			t.Errorf("request %d started %v after the previous one, want at least ~%v", i, gap, interval)
		}
	}
}

func TestClickUpClient_WithMinInterval_ZeroDoesNotPace(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id": "abc123"}`))
	}))
	defer server.Close()

	client := New("my-key").WithBaseURL(server.URL)
	begin := time.Now()
	for i := 0; i < 3; i++ {
		if _, err := client.GetTask(context.Background(), "abc123"); err != nil {
			t.Fatalf("GetTask failed: %v", err)
		}
	}
	if elapsed := time.Since(begin); elapsed > time.Second {
		t.Errorf("expected unpaced requests to be fast, took %v", elapsed)
	}
}

func TestClickUpClient_WithMinInterval_CancelDuringWait(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"id": "abc123"}`))
	}))
	defer server.Close()

	client := New("my-key").WithBaseURL(server.URL).WithMinInterval(5 * time.Second)
	if _, err := client.GetTask(context.Background(), "abc123"); err != nil {
		t.Fatalf("first GetTask failed: %v", err)
	}

	client.paceMu.Lock()
	freeAt := client.nextSlot
	client.paceMu.Unlock()

	// The second request must wait ~5s for its slot; cancelling should end
	// the wait promptly without sending it.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	begin := time.Now()
	_, err := client.GetTask(ctx, "abc123")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected context.DeadlineExceeded, got %v", err)
	}
	if elapsed := time.Since(begin); elapsed > 2*time.Second {
		t.Errorf("expected cancelled wait to return promptly, took %v", elapsed)
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("expected the cancelled request not to be sent, server saw %d requests", got)
	}
	// Nobody queued behind it, so its slot is handed back.
	client.paceMu.Lock()
	defer client.paceMu.Unlock()
	if !client.nextSlot.Equal(freeAt) {
		t.Errorf("expected the cancelled slot to be released (next slot %v), got %v", freeAt, client.nextSlot)
	}
}

// TestClickUpClient_RateLimitHoldsAllRequests checks that a 429's Retry-After
// holds back the next request on the same client, whoever sends it: the MCP
// tools and the sync worker share one client and one rate limit.
func TestClickUpClient_RateLimitHoldsAllRequests(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Retry-After", "5")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"id": "abc123"}`))
	}))
	defer server.Close()

	client := New("my-key").WithBaseURL(server.URL) // no pacing
	var rateLimitErr *RateLimitError
	if _, err := client.GetTask(context.Background(), "abc123"); !errors.As(err, &rateLimitErr) {
		t.Fatalf("expected a RateLimitError, got %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := client.GetTask(ctx, "abc123"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected the next request to wait out Retry-After, got %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("expected no request during the Retry-After window, server saw %d", got)
	}
}
