package clickup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
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
		if err != tt.expected {
			t.Errorf("expected error %v for status %d, got %v", tt.expected, tt.status, err)
		}
		server.Close()
	}
}
