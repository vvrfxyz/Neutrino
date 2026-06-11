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

// New panels send "code" + "permanent" alongside the legacy error string; the
// structured flag must take precedence over the string tables.
func TestPanelClientPushUsagePrefersStructuredPermanentField(t *testing.T) {
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

	// permanent=true with an unknown string: skipped like an accepted event
	// (legacy classification would have produced a UsageRejectionError).
	if err := push(t, `{"processed":1,"results":[{"user_id":1,"error":"some new rejection","code":"new_code","permanent":true}]}`); err != nil {
		t.Fatalf("permanent=true must be skipped, got %v", err)
	}

	// permanent=false with a string the legacy table calls permanent: the
	// structured field wins → plain retryable error, not skipped, and not a
	// deterministic rejection.
	err := push(t, `{"processed":1,"results":[{"user_id":1,"error":"user not found","code":"user_not_found","permanent":false}]}`)
	if err == nil {
		t.Fatalf("permanent=false must surface an error")
	}
	if isRejection(err) {
		t.Fatalf("permanent=false must not be a UsageRejectionError: %v", err)
	}

	// Without the structured field, legacy string matching still applies.
	if err := push(t, `{"processed":1,"results":[{"user_id":1,"error":"user not found"}]}`); err != nil {
		t.Fatalf("legacy permanent string must be skipped, got %v", err)
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
