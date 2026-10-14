package oauthflow

import (
	"errors"
	"fmt"
	"os"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func errf(format string, a ...any) error { return fmt.Errorf(format, a...) }

// ErrCancelled is returned when a sign-in is cancelled.
var ErrCancelled = errors.New("oauth: sign-in cancelled")

// Well-known provider endpoints and scopes. Client credentials are supplied by
// the caller (see DefaultGoogleClientID for the built-in Google default).

const (
	// DefaultGoogleClientID / Secret are the well-known public credentials used
	// by rclone. They let Google Drive work with no setup, at the cost of
	// sharing Google's per-project quota with other rclone users. Supply your
	// own client to avoid that. Override at build time, e.g.:
	//   go build -ldflags "-X qkmount/internal/oauthflow.DefaultGoogleClientID=... -X qkmount/internal/oauthflow.DefaultGoogleClientSecret=..."
	GoogleAuthURL  = "https://accounts.google.com/o/oauth2/auth"
	GoogleTokenURL = "https://oauth2.googleapis.com/token"
)

// These are vars (not consts) so a vendor can bake in their own OAuth client
// with -ldflags -X when building a branded distribution.
var (
	DefaultGoogleClientID     = "202264815644.apps.googleusercontent.com"
	DefaultGoogleClientSecret = "X4Z3ca8xfWDb1Voo-F9a7ZxJ"
)

// GoogleDriveScopes requests full Drive access, matching what a mounted drive
// needs (read, write, create, delete, list).
var GoogleDriveScopes = []string{"https://www.googleapis.com/auth/drive"}

const (
	DropboxAuthURL  = "https://www.dropbox.com/oauth2/authorize"
	DropboxTokenURL = "https://api.dropboxapi.com/oauth2/token"

	MicrosoftAuthURL  = "https://login.microsoftonline.com/common/oauth2/v2.0/authorize"
	MicrosoftTokenURL = "https://login.microsoftonline.com/common/oauth2/v2.0/token"
)

// OneDriveScopes requests read/write access to the user's files plus a refresh
// token.
var OneDriveScopes = []string{
	"offline_access",
	"https://graph.microsoft.com/Files.ReadWrite.All",
}

// DropboxScopes is empty because Dropbox uses scoped access configured on the
// app itself; tokens are requested without an explicit scope list.
var DropboxScopes = []string{}

// Provider describes a ready-to-run OAuth flow for a service.
type Provider struct {
	// Kind is QKMount's backend kind ("gdrive", "dropbox", "onedrive").
	Kind string
	// Label is a human name for messages.
	Label string
	// Params are the flow parameters (endpoints, scopes, credentials).
	Params Params
}

// ProviderParams returns the OAuth flow for a built-in provider. Dropbox and
// OneDrive require an app registration, so their credentials must be supplied
// via the environment; Google works out of the box.
func ProviderParams(kind string) (Provider, error) {
	switch kind {
	case "gdrive", "drive", "google":
		id := envOr("QKMOUNT_GOOGLE_CLIENT_ID", DefaultGoogleClientID)
		secret := envOr("QKMOUNT_GOOGLE_CLIENT_SECRET", DefaultGoogleClientSecret)
		return Provider{
			Kind:  "gdrive",
			Label: "Google Drive",
			Params: Params{
				ClientID:     id,
				ClientSecret: secret,
				AuthURL:      GoogleAuthURL,
				TokenURL:     GoogleTokenURL,
				Scopes:       GoogleDriveScopes,
			},
		}, nil
	case "dropbox":
		id := envOr("QKMOUNT_DROPBOX_KEY", "")
		secret := envOr("QKMOUNT_DROPBOX_SECRET", "")
		if id == "" {
			return Provider{}, errf("Dropbox needs an app key; set QKMOUNT_DROPBOX_KEY and QKMOUNT_DROPBOX_SECRET (create one at dropbox.com/developers/apps)")
		}
		return Provider{
			Kind:  "dropbox",
			Label: "Dropbox",
			Params: Params{
				ClientID:     id,
				ClientSecret: secret,
				AuthURL:      DropboxAuthURL,
				TokenURL:     DropboxTokenURL,
				Scopes:       DropboxScopes,
			},
		}, nil
	case "onedrive", "microsoft":
		id := envOr("QKMOUNT_ONEDRIVE_CLIENT_ID", "")
		secret := envOr("QKMOUNT_ONEDRIVE_CLIENT_SECRET", "")
		if id == "" {
			return Provider{}, errf("OneDrive needs an application (client) ID; set QKMOUNT_ONEDRIVE_CLIENT_ID and QKMOUNT_ONEDRIVE_CLIENT_SECRET (register an app in Azure)")
		}
		return Provider{
			Kind:  "onedrive",
			Label: "OneDrive",
			Params: Params{
				ClientID:     id,
				ClientSecret: secret,
				AuthURL:      MicrosoftAuthURL,
				TokenURL:     MicrosoftTokenURL,
				Scopes:       OneDriveScopes,
			},
		}, nil
	default:
		return Provider{}, errf("unknown provider %q (try gdrive, dropbox or onedrive)", kind)
	}
}
