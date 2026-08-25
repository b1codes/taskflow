package clickup

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClient_UpdateTaskStatus(t *testing.T) {
	t.Parallel()

	var receivedReq updateStatusRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" || r.URL.Path != "/task/task-update" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &receivedReq)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := New("api-key").WithBaseURL(server.URL)
	err := client.UpdateTaskStatus(context.Background(), "task-update", "completed")
	if err != nil {
		t.Fatalf("UpdateTaskStatus failed: %v", err)
	}
	if receivedReq.Status != "completed" {
		t.Errorf("expected status 'completed', got %q", receivedReq.Status)
	}
}

func TestClient_PostComment(t *testing.T) {
	t.Parallel()

	var receivedReq postCommentRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/task/task-comment/comment" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &receivedReq)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := New("api-key").WithBaseURL(server.URL)
	err := client.PostComment(context.Background(), "task-comment", "Hello world comment")
	if err != nil {
		t.Fatalf("PostComment failed: %v", err)
	}
	if receivedReq.CommentText != "Hello world comment" {
		t.Errorf("expected comment 'Hello world comment', got %q", receivedReq.CommentText)
	}
}
