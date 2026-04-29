package app

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"neutrino/internal/config"
)

func TestRoutesUsageRejectsAPIKeyAuth(t *testing.T) {
	a := &App{cfg: config.Config{APIKeyHeader: "X-API-Key"}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/usage", nil)
	req.Header.Set("X-API-Key", "pre-provisioned-key")
	rr := httptest.NewRecorder()

	a.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rr.Code)
	}
}
