package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPanelClientPushUsageAcceptsPermanentPartialRejections(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/usage" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"processed":2,"results":[{"user_id":1,"status":"over_limit"},{"user_id":1,"error":"user not active"}]}`))
	}))
	defer srv.Close()

	client := &PanelClient{
		baseURL: srv.URL,
		client:  srv.Client(),
	}
	if err := client.PushUsage(context.Background(), []UsageEvent{
		{UserID: 1, Direction: "outbound", Bytes: 1, Source: "test", SourceEventID: "a"},
		{UserID: 1, Direction: "inbound", Bytes: 1, Source: "test", SourceEventID: "b"},
	}); err != nil {
		t.Fatalf("PushUsage: %v", err)
	}
}

func TestPanelClientPushUsageAcceptsTooOldTimestampRejection(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"processed":1,"results":[{"user_id":1,"error":"event timestamp too old"}]}`))
	}))
	defer srv.Close()

	client := &PanelClient{
		baseURL: srv.URL,
		client:  srv.Client(),
	}
	if err := client.PushUsage(context.Background(), []UsageEvent{
		{UserID: 1, Direction: "outbound", Bytes: 1, Source: "test", SourceEventID: "old"},
	}); err != nil {
		t.Fatalf("PushUsage: %v", err)
	}
}

func TestPanelClientPushUsageRejectsOutsideCurrentWindowRejection(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"processed":1,"results":[{"user_id":1,"error":"event outside current quota window"}]}`))
	}))
	defer srv.Close()

	client := &PanelClient{
		baseURL: srv.URL,
		client:  srv.Client(),
	}
	err := client.PushUsage(context.Background(), []UsageEvent{
		{UserID: 1, Direction: "outbound", Bytes: 1, Source: "test", SourceEventID: "old-window"},
	})
	if err == nil {
		t.Fatalf("expected outside-current-window rejection to remain retryable")
	}
	if !strings.Contains(err.Error(), "event outside current quota window") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPanelClientPushUsageRejectsTransientPartialBatchResult(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/usage" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"processed":2,"results":[{"user_id":1,"status":"active"},{"user_id":1,"error":"record failed"}]}`))
	}))
	defer srv.Close()

	client := &PanelClient{
		baseURL: srv.URL,
		client:  srv.Client(),
	}
	err := client.PushUsage(context.Background(), []UsageEvent{
		{UserID: 1, Direction: "outbound", Bytes: 1, Source: "test", SourceEventID: "a"},
		{UserID: 1, Direction: "inbound", Bytes: 1, Source: "test", SourceEventID: "b"},
	})
	if err == nil {
		t.Fatalf("expected transient partial rejection error")
	}
	if !strings.Contains(err.Error(), "record failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPanelClientPushUsageAcceptsDuplicateResults(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"processed":2,"results":[{"user_id":1,"status":"active"},{"user_id":1,"status":"duplicate","deduplicated":true}]}`))
	}))
	defer srv.Close()

	client := &PanelClient{
		baseURL: srv.URL,
		client:  srv.Client(),
	}
	if err := client.PushUsage(context.Background(), []UsageEvent{
		{UserID: 1, Direction: "outbound", Bytes: 1, Source: "test", SourceEventID: "a"},
		{UserID: 1, Direction: "inbound", Bytes: 1, Source: "test", SourceEventID: "b"},
	}); err != nil {
		t.Fatalf("PushUsage: %v", err)
	}
}
