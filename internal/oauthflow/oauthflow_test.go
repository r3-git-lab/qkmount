package oauthflow

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestRunCompletesFlow runs the loopback flow end to end against mock
// authorization and token endpoints. The "browser" is simulated by parsing the
// redirect_uri/state out of the consent URL and calling it with a code.
func TestRunCompletesFlow(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "authorization_code" {
			t.Errorf("grant_type = %q", r.Form.Get("grant_type"))
		}
		if r.Form.Get("code") != "test-code" {
			t.Errorf("code = %q", r.Form.Get("code"))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-123",
			"token_type":    "Bearer",
			"refresh_token": "refresh-456",
			"expires_in":    3600,
		})
	}))
	defer tokenSrv.Close()

	params := Params{
		ClientID:     "client",
		ClientSecret: "secret",
		AuthURL:      "https://auth.example/authorize",
		TokenURL:     tokenSrv.URL,
		Scopes:       []string{"scope1"},
		Timeout:      10 * time.Second,
	}

	// The fake browser: when the consent URL is produced, hit the loopback
	// redirect with the right state and a code.
	fakeBrowser := func(authURL string) error {
		u, err := url.Parse(authURL)
		if err != nil {
			return err
		}
		q := u.Query()
		redirect := q.Get("redirect_uri")
		state := q.Get("state")
		go func() {
			cb, _ := url.Parse(redirect)
			rq := cb.Query()
			rq.Set("code", "test-code")
			rq.Set("state", state)
			cb.RawQuery = rq.Encode()
			resp, err := http.Get(cb.String())
			if err == nil {
				resp.Body.Close()
			}
		}()
		return nil
	}

	res, err := Run(context.Background(), params, fakeBrowser)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Token.AccessToken != "access-123" || res.Token.RefreshToken != "refresh-456" {
		t.Fatalf("token = %+v", res.Token)
	}

	// TokenJSON must round-trip through the shape the backend parses.
	js, err := TokenJSON(res.Token)
	if err != nil {
		t.Fatalf("token json: %v", err)
	}
	if !strings.Contains(js, "refresh-456") {
		t.Fatalf("token json missing refresh token: %s", js)
	}
}

func TestProviderParamsGoogle(t *testing.T) {
	p, err := ProviderParams("gdrive")
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != "gdrive" || p.Params.ClientID == "" || len(p.Params.Scopes) == 0 {
		t.Fatalf("bad provider: %+v", p)
	}
	t.Setenv("QKMOUNT_GOOGLE_CLIENT_ID", "custom-id")
	t.Setenv("QKMOUNT_GOOGLE_CLIENT_SECRET", "custom-secret")
	p2, err := ProviderParams("google")
	if err != nil {
		t.Fatal(err)
	}
	if p2.Params.ClientID != "custom-id" || p2.Params.ClientSecret != "custom-secret" {
		t.Fatalf("env override ignored: %+v", p2.Params)
	}
}

func TestProviderParamsDropboxRequiresKey(t *testing.T) {
	t.Setenv("QKMOUNT_DROPBOX_KEY", "")
	if _, err := ProviderParams("dropbox"); err == nil {
		t.Fatal("expected error without QKMOUNT_DROPBOX_KEY")
	}
	t.Setenv("QKMOUNT_DROPBOX_KEY", "appkey")
	p, err := ProviderParams("dropbox")
	if err != nil {
		t.Fatal(err)
	}
	if p.Params.ClientID != "appkey" {
		t.Fatalf("got %q", p.Params.ClientID)
	}
}

func TestRunRejectsStateMismatch(t *testing.T) {
	params := Params{
		ClientID:     "client",
		ClientSecret: "secret",
		AuthURL:      "https://auth.example/authorize",
		TokenURL:     "https://token.example",
		Scopes:       []string{"s"},
		Timeout:      2 * time.Second,
	}
	fakeBrowser := func(authURL string) error {
		u, _ := url.Parse(authURL)
		redirect := u.Query().Get("redirect_uri")
		go func() {
			cb, _ := url.Parse(redirect)
			rq := cb.Query()
			rq.Set("code", "c")
			rq.Set("state", "WRONG")
			cb.RawQuery = rq.Encode()
			resp, err := http.Get(cb.String())
			if err == nil {
				resp.Body.Close()
			}
		}()
		return nil
	}
	_, err := Run(context.Background(), params, fakeBrowser)
	if err == nil {
		t.Fatal("expected timeout/error for state mismatch")
	}
}
