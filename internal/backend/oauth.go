package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"qkmount/internal/oauthflow"
)

// tokenJSON mirrors the on-disk token shape used by rclone and other tools.
type tokenJSON struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	RefreshToken string `json:"refresh_token"`
	Expiry       string `json:"expiry"`
	ExpiresIn    int64  `json:"expires_in"`
}

// parseOAuthToken accepts a raw access token, a token JSON object, or a
// {"token": {...}} wrapper (as produced by rclone).
func parseOAuthToken(raw string) (*oauth2.Token, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("no OAuth token provided")
	}
	if strings.HasPrefix(raw, "{") {
		var probe map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &probe); err == nil {
			if t, ok := probe["token"]; ok {
				raw = string(t)
			}
		}
		var tj tokenJSON
		if err := json.Unmarshal([]byte(raw), &tj); err == nil && tj.AccessToken != "" {
			tok := &oauth2.Token{
				AccessToken:  tj.AccessToken,
				TokenType:    tj.TokenType,
				RefreshToken: tj.RefreshToken,
			}
			if tok.TokenType == "" {
				tok.TokenType = "Bearer"
			}
			if tj.Expiry != "" {
				if exp, err := time.Parse(time.RFC3339, tj.Expiry); err == nil {
					tok.Expiry = exp
				}
			} else if tj.ExpiresIn > 0 {
				tok.Expiry = time.Now().Add(time.Duration(tj.ExpiresIn) * time.Second)
			}
			return tok, nil
		}
	}
	// Bare access token.
	return &oauth2.Token{AccessToken: raw, TokenType: "Bearer"}, nil
}

// oauthHTTPClient returns an http.Client that adds the bearer token and
// refreshes it when possible.
func oauthHTTPClient(ctx context.Context, tok *oauth2.Token, clientID, clientSecret, authURL, tokenURL string) *http.Client {
	var src oauth2.TokenSource
	if clientID != "" && tokenURL != "" {
		cfg := &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			Endpoint:     oauth2.Endpoint{AuthURL: authURL, TokenURL: tokenURL},
		}
		src = cfg.TokenSource(ctx, tok)
	} else {
		// No credentials to refresh with; use the token as-is.
		src = oauth2.StaticTokenSource(tok)
	}
	return oauth2.NewClient(ctx, src)
}

// Default public client credentials for Google Drive. These match the
// well-known rclone defaults so tokens exported from rclone keep refreshing.
// Users can always supply their own OAuth client in the connection options.
var (
	DefaultGoogleClientID     = oauthflow.DefaultGoogleClientID
	DefaultGoogleClientSecret = oauthflow.DefaultGoogleClientSecret

	defaultGoogleClientID     = oauthflow.DefaultGoogleClientID
	defaultGoogleClientSecret = oauthflow.DefaultGoogleClientSecret
)

// ErrRateLimited marks a request rejected because a provider quota or rate
// limit was hit. Unlike a true permission error it is usually transient, so
// callers retry with backoff and the FUSE layer reports it as an I/O error
// rather than a misleading "permission denied".
var ErrRateLimited = errors.New("rate limited by provider")

// ErrNotDownloadable marks content the provider refuses to serve as raw bytes,
// such as a Google Docs Editors file requested without an export format.
var ErrNotDownloadable = errors.New("file cannot be downloaded directly")

// rateLimitCooldown is how long QKMount stops calling a provider after it
// reports a quota error. Shared OAuth clients (like rclone's Google client)
// have very small per-minute budgets, so backing off avoids wasting the budget
// and turning one transient limit into a storm of failures.
const rateLimitCooldown = 12 * time.Second

var (
	rlMu    sync.Mutex
	rlUntil time.Time
)

// checkRateLimitCooldown returns ErrRateLimited while a cooldown is active.
func checkRateLimitCooldown() error {
	rlMu.Lock()
	defer rlMu.Unlock()
	if until := rlUntil; time.Now().Before(until) {
		return fmt.Errorf("%w: provider quota exceeded; retrying in %s", ErrRateLimited, time.Until(until).Round(time.Second))
	}
	return nil
}

func noteRateLimited() {
	rlMu.Lock()
	rlUntil = time.Now().Add(rateLimitCooldown)
	rlMu.Unlock()
}

func resetRateLimit() {
	rlMu.Lock()
	rlUntil = time.Time{}
	rlMu.Unlock()
}

// httpErrorBody builds an error for a non-2xx response given its body.
func httpErrorBody(statusCode int, status string, body []byte) error {
	msg := strings.TrimSpace(string(body))
	switch statusCode {
	case 401:
		return fmt.Errorf("%w: %s", os.ErrPermission, msg)
	case 403, 429:
		if statusCode == 429 || isRateLimitReason(providerReason(body)) {
			return fmt.Errorf("%w: %s", ErrRateLimited, truncate(msg, 300))
		}
		if strings.Contains(msg, "Only files with binary content") {
			return fmt.Errorf("%w: %s", ErrNotDownloadable, truncate(msg, 200))
		}
		return fmt.Errorf("%w: %s", os.ErrPermission, truncate(msg, 300))
	case 404:
		return fmt.Errorf("%w: %s", os.ErrNotExist, msg)
	case 409:
		return fmt.Errorf("%w: %s", os.ErrExist, msg)
	}
	return fmt.Errorf("http %s: %s", status, truncate(msg, 200))
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// providerReason extracts a Google-style error reason from a JSON error body.
func providerReason(body []byte) string {
	var e struct {
		Error struct {
			Status string `json:"status"`
			Errors []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		return ""
	}
	if len(e.Error.Errors) > 0 && e.Error.Errors[0].Reason != "" {
		return e.Error.Errors[0].Reason
	}
	return e.Error.Status
}

func isRateLimitReason(reason string) bool {
	switch reason {
	case "rateLimitExceeded", "userRateLimitExceeded", "quotaExceeded",
		"dailyLimitExceeded", "RESOURCE_EXHAUSTED", "TooManyRequests":
		return true
	}
	return false
}

// httpError turns a non-2xx response into a descriptive error. It consumes the
// body, so callers that need it must read it first.
func httpError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return httpErrorBody(resp.StatusCode, resp.Status, body)
}

// ---- optional store capabilities -----------------------------------------

// DirectoryStore is implemented by stores with real (non-prefix) directories.
type DirectoryStore interface {
	Mkdir(dir string) error
	RemoveDir(dir string) error
}

// RenameStore is implemented by stores that can rename server-side.
type RenameStore interface {
	RenameObject(src, dst string) error
}

// StatDirStore is implemented by stores that can tell a file from a
// (possibly empty) directory for a single key without listing it.
type StatDirStore interface {
	StatDir(key string) (isDir bool, err error)
}

// debugHTTP enables verbose HTTP error logging when QKMOUNT_DEBUG_HTTP is set.
var debugHTTP = os.Getenv("QKMOUNT_DEBUG_HTTP") != ""

// putJSON performs a JSON request and decodes the response into out (if non-nil).
// Provider rate limits are retried once; if they persist a short cooldown is
// set so a saturated shared client does not get hammered (which would only
// waste its small per-minute budget).
func putJSON(client *http.Client, method, url string, body any, out any, headers map[string]string) error {
	var payload []byte
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = raw
	}

	// Fail fast while a cooldown from a recent quota error is in effect.
	if err := checkRateLimitCooldown(); err != nil {
		return err
	}

	const maxAttempts = 2
	for attempt := 0; attempt < maxAttempts; attempt++ {
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(payload)
		}
		req, err := http.NewRequest(method, url, rd)
		if err != nil {
			return err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode >= 300 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			herr := httpErrorBody(resp.StatusCode, resp.Status, b)
			if debugHTTP {
				fmt.Fprintf(os.Stderr, "qkmount[debug]: %s %s -> %s: %s\n", method, url, resp.Status, truncate(strings.TrimSpace(string(b)), 300))
			}
			if errors.Is(herr, ErrRateLimited) {
				if attempt < maxAttempts-1 {
					time.Sleep(800 * time.Millisecond)
					continue
				}
				noteRateLimited()
				return herr
			}
			return herr
		}
		resetRateLimit()
		defer resp.Body.Close()
		if out != nil {
			return json.NewDecoder(resp.Body).Decode(out)
		}
		return nil
	}
	return nil
}
