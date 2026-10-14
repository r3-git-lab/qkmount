// Package oauthflow implements the "install app" OAuth 2.0 flow used by
// QKMount: it opens the user's browser, runs a short-lived loopback HTTP
// server to catch the redirect, and exchanges the authorization code for a
// token. This is what lets a non-technical user sign in to Google/Dropbox/etc.
// without ever seeing a client ID, secret, or token JSON.
package oauthflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// Params describes a single authorization flow.
type Params struct {
	ClientID     string
	ClientSecret string
	AuthURL      string
	TokenURL     string
	Scopes       []string
	// Timeout bounds how long we wait for the user to finish in the browser.
	Timeout time.Duration
}

// Result is a completed flow.
type Result struct {
	Token *oauth2.Token
}

// TokenJSON serialises a token in the shape QKMount stores and re-reads.
func TokenJSON(tok *oauth2.Token) (string, error) {
	b, err := json.Marshal(tok)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func randState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Run executes the flow. openURL (usually a browser launcher) is called with
// the consent URL; if it returns an error the URL is still printed by callers
// so the user can paste it manually. The function blocks until the user
// finishes, the context is cancelled, or the timeout elapses.
func Run(ctx context.Context, p Params, openURL func(string) error) (*Result, error) {
	if p.ClientID == "" {
		return nil, fmt.Errorf("oauth: no client ID configured")
	}
	if p.Timeout <= 0 {
		p.Timeout = 5 * time.Minute
	}
	if len(p.Scopes) == 0 {
		return nil, fmt.Errorf("oauth: no scopes configured")
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("oauth: cannot listen on loopback: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	redirectURL := fmt.Sprintf("http://127.0.0.1:%d/", port)

	cfg := &oauth2.Config{
		ClientID:     p.ClientID,
		ClientSecret: p.ClientSecret,
		Endpoint:     oauth2.Endpoint{AuthURL: p.AuthURL, TokenURL: p.TokenURL},
		RedirectURL:  redirectURL,
		Scopes:       p.Scopes,
	}

	state, err := randState()
	if err != nil {
		return nil, err
	}
	// access_type=offline + prompt=consent make Google (and friends) return a
	// refresh token so the mount keeps working after the access token expires.
	authURL := cfg.AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("prompt", "consent"),
	)

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			msg := e
			if d := q.Get("error_description"); d != "" {
				msg = d
			}
			fmt.Fprint(w, resultPage(false, msg))
			select {
			case errCh <- fmt.Errorf("oauth: authorization denied: %s", msg):
			default:
			}
			return
		}
		if q.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, resultPage(true, ""))
		select {
		case codeCh <- code:
		default:
		}
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	if openURL != nil {
		_ = openURL(authURL)
	}

	timer := time.NewTimer(p.Timeout)
	defer timer.Stop()

	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		return nil, err
	case <-timer.C:
		return nil, fmt.Errorf("oauth: timed out waiting for sign-in (open %s)", authURL)
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	tok, err := cfg.Exchange(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("oauth: token exchange failed: %w", err)
	}
	if tok.RefreshToken == "" {
		// Not fatal, but the mount will stop working once the access token
		// expires; surface it rather than silently returning a weak token.
		return &Result{Token: tok}, fmt.Errorf("oauth: no refresh token returned; re-run sign-in and approve offline access")
	}
	return &Result{Token: tok}, nil
}

// OpenBrowser tries to open url in the user's default browser. On Linux it
// honours $BROWSER when set.
func OpenBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return startDetached("open", url)
	case "windows":
		return startDetached("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		if b := os.Getenv("BROWSER"); b != "" {
			// $BROWSER may contain arguments, e.g. "firefox --new-window".
			fields := strings.Fields(b)
			return startDetached(fields[0], append(fields[1:], url)...)
		}
		return startDetached("xdg-open", url)
	}
}

func resultPage(ok bool, msg string) string {
	title, color, heading, body := "QKMount", "#16a34a", "You're signed in", "You can close this tab and return to QKMount."
	if !ok {
		title, color, heading = "QKMount", "#dc2626", "Sign-in failed"
		body = "Return to QKMount and try again. Details: " + msg
	}
	body = strings.ReplaceAll(body, "<", "&lt;")
	return `<!doctype html><html><head><meta charset="utf-8"><title>` + title +
		`</title></head><body style="font-family:system-ui,sans-serif;background:#0f172a;color:#e2e8f0;` +
		`display:flex;align-items:center;justify-content:center;height:100vh;margin:0">` +
		`<div style="text-align:center;max-width:32rem;padding:2rem">` +
		`<h1 style="color:` + color + `">` + heading + `</h1><p>` + body + `</p></div></body></html>`
}
