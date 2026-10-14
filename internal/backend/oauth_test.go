package backend

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
)

func TestHTTPErrorBodyRateLimit(t *testing.T) {
	rateBody := []byte(`{"error":{"code":403,"message":"Quota exceeded","errors":[{"reason":"rateLimitExceeded"}]}}`)
	err := httpErrorBody(403, "403 Forbidden", rateBody)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("rate-limit 403 not mapped to ErrRateLimited: %v", err)
	}
	if errors.Is(err, os.ErrPermission) {
		t.Fatalf("rate-limit 403 must not look like a permission error: %v", err)
	}

	permBody := []byte(`{"error":{"code":403,"message":"The user does not have sufficient permissions","errors":[{"reason":"insufficientFilePermissions"}]}}`)
	err = httpErrorBody(403, "403 Forbidden", permBody)
	if errors.Is(err, ErrRateLimited) {
		t.Fatalf("permission 403 wrongly mapped to rate limit: %v", err)
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("permission 403 not mapped to os.ErrPermission: %v", err)
	}

	if !errors.Is(httpErrorBody(429, "429 Too Many Requests", nil), ErrRateLimited) {
		t.Fatal("429 not mapped to ErrRateLimited")
	}
}

// TestPutJSONRetriesRateLimit verifies that a transient rate limit is retried
// once before giving up.
func TestPutJSONRetriesRateLimit(t *testing.T) {
	resetRateLimit()
	defer resetRateLimit()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 2 {
			w.WriteHeader(403)
			w.Write([]byte(`{"error":{"errors":[{"reason":"rateLimitExceeded"}]}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	var out struct {
		OK bool `json:"ok"`
	}
	if err := putJSON(srv.Client(), "POST", srv.URL, map[string]any{"a": 1}, &out, nil); err != nil {
		t.Fatalf("putJSON: %v", err)
	}
	if !out.OK {
		t.Fatal("response not decoded after retry")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected one retry (2 calls), got %d", got)
	}
}

// TestRateLimitCooldownShortCircuits verifies that once a provider reports a
// quota error, further calls fail fast without touching the network until the
// cooldown expires.
func TestRateLimitCooldownShortCircuits(t *testing.T) {
	resetRateLimit()
	noteRateLimited()
	defer resetRateLimit()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request made while rate-limit cooldown was active")
	}))
	defer srv.Close()

	err := putJSON(srv.Client(), "GET", srv.URL, nil, nil, nil)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited during cooldown, got %v", err)
	}
}
