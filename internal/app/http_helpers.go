package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

func (a *App) writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func (a *App) readJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// apiError writes the canonical /api/v1 error envelope {"error": message}.
// Every JSON API error goes through here so clients can rely on one shape;
// SSR pages and the public /sub endpoint keep plain-text errors. The per-event
// results inside POST /api/v1/usage are a separate agent wire contract and do
// not use this envelope.
func (a *App) apiError(w http.ResponseWriter, status int, message string) {
	a.writeJSON(w, status, map[string]any{"error": message})
}

// withID adapts a handler that needs the numeric {id} path wildcard of a
// method+pattern route. Non-numeric or non-positive ids get a 400 with the
// given message ("bad node id", "bad user id", ...).
func (a *App) withID(badIDMsg string, h func(http.ResponseWriter, *http.Request, int64)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := parseInt64Path(r.PathValue("id"))
		if err != nil {
			a.apiError(w, http.StatusBadRequest, badIDMsg)
			return
		}
		h(w, r, id)
	}
}

func parseInt64Path(part string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid id")
	}
	return id, nil
}

// setHXToast emits an htmx trigger in a response header.
// Header values are effectively ASCII-only across proxies; use QuoteToASCII
// so Chinese messages don't get mojibake.
func setHXToast(w http.ResponseWriter, level, message string) {
	w.Header().Set(
		"HX-Trigger",
		fmt.Sprintf(
			`{"toast":{"level":%s,"message":%s}}`,
			strconv.QuoteToASCII(level),
			strconv.QuoteToASCII(message),
		),
	)
}

func setHXBoolTrigger(w http.ResponseWriter, name string) {
	w.Header().Set(
		"HX-Trigger",
		fmt.Sprintf(`{%s:true}`, strconv.QuoteToASCII(name)),
	)
}

func readAllLimit(r io.Reader, limit int64) ([]byte, error) {
	lr := &io.LimitedReader{R: r, N: limit + 1}
	b, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("body too large")
	}
	return b, nil
}

func requestIsHTTPS(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.TLS != nil {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Ssl")), "on") {
		return true
	}
	return false
}
