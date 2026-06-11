package agent

import (
	"context"
	"errors"
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

func TestPanelClientPushUsageClassifiesRejectionTypes(t *testing.T) {
	t.Parallel()

	push := func(t *testing.T, body string) error {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
		defer srv.Close()
		client := &PanelClient{baseURL: srv.URL, client: srv.Client()}
		return client.PushUsage(context.Background(), []UsageEvent{
			{UserID: 1, Direction: "outbound", Bytes: 1, Source: "test", SourceEventID: "a"},
		})
	}
	isRejection := func(err error) bool {
		var rej *UsageRejectionError
		return errors.As(err, &rej)
	}

	// Transient panel states must not be typed as deterministic rejections,
	// or the flush loop would count them toward poison-batch quarantine.
	for _, msg := range []string{"record failed", "invalid event timestamp"} {
		err := push(t, `{"processed":1,"results":[{"user_id":1,"error":"`+msg+`"}]}`)
		if err == nil {
			t.Fatalf("%s: expected error", msg)
		}
		if isRejection(err) {
			t.Fatalf("%s: must not be a UsageRejectionError", msg)
		}
	}

	// Unknown rejection strings are deterministic per batch content.
	err := push(t, `{"processed":1,"results":[{"user_id":1,"error":"event outside current quota window"}]}`)
	if !isRejection(err) {
		t.Fatalf("expected UsageRejectionError, got %v", err)
	}

	// Response-shape mismatch is a deterministic contract violation.
	err = push(t, `{"processed":2,"results":[{"user_id":1,"status":"active"},{"user_id":2,"status":"active"}]}`)
	if !isRejection(err) {
		t.Fatalf("count mismatch: expected UsageRejectionError, got %v", err)
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
