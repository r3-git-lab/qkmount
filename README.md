# QKMount

Mount cloud storage and remote servers as ordinary local folders — on Linux,
macOS and Windows — from a single lightweight binary with a small web UI and a
native tray icon.

QKMount is written from scratch in Go. Each mount runs as a supervised child
process, so a dropped connection is detected and **automatically re-mounted**
with backoff instead of leaving you with a dead folder.

```
$ qkmount import-rclone          # reuse your existing rclone remotes
$ qkmount ui                     # open the UI, start the daemon
```

## Highlights

- **One small binary** (~12 MB, no runtime, no Python/Node).
- **Doesn't lose mounts.** A supervisor watches every mount and reconnects
  automatically (exponential backoff, capped at 60 s).
- **Native tray** (StatusNotifierItem/DBus) with per-connection mount toggles,
  plus an app launcher and login autostart.
- **Embedded web UI**, styled after Mountain Duck's connection list.
- **Import from rclone** — including OAuth tokens and de-obfuscated passwords —
  so existing Drive/Dropbox/OneDrive/SFTP logins keep working.
- **Uniform behaviour** across providers via a shared object-store layer for
  key/value services and a direct layer for filesystem-style services.

## Supported services

| Type | Key | Notes |
|------|-----|-------|
| Local folder | `local` | Useful for testing and for "pass-through" mounts |
| SFTP | `sftp` | Password or private key |
| FTP / FTPS | `ftp` | Plain or explicit TLS |
| WebDAV | `webdav` | Nextcloud, ownCloud, Apache mod_dav, etc. |
| S3-compatible | `s3` | AWS S3, MinIO, Wasabi, DigitalOcean Spaces, … |
| Google Drive | `gdrive` | OAuth 2 |
| Dropbox | `dropbox` | OAuth 2 |
| Microsoft OneDrive | `onedrive` | Microsoft Graph |
| Backblaze B2 | `b2` | Native B2 API |
| Azure Blob Storage | `azure` | Shared-key auth |
| SMB / Windows share | `smb` | via go-smb2 |

## Install

### Prebuilt binaries

Every tagged [release](https://github.com/r3-git-lab/qkmount/releases) ships
ready-to-run binaries, so you don't need a Go toolchain:

| OS | Architecture | Asset |
| --- | --- | --- |
| Linux | x86-64 | `qkmount_<ver>_linux_amd64.tar.gz` |
| Linux | arm64 | `qkmount_<ver>_linux_arm64.tar.gz` |
| macOS | Apple silicon | `qkmount_<ver>_darwin_arm64.tar.gz` |
| macOS | Intel | `qkmount_<ver>_darwin_amd64.tar.gz` |
| Windows | x86-64 | `qkmount_<ver>_windows_amd64.zip` |
| Windows | arm64 | `qkmount_<ver>_windows_arm64.zip` |

Download the asset for your platform, unpack it, and put `qkmount` (or
`qkmount.exe`) on your `PATH`. A `SHA256SUMS.txt` is published alongside the
assets so you can verify the download. You only need the **runtime** FUSE layer
for your OS — `libfuse3` (Linux), [macFUSE](https://macfuse.github.io/) (macOS)
or [WinFsp](https://winfsp.dev/) (Windows).

### Build requirements

QKMount links to the platform FUSE implementation through cgo:

- **Linux** — `libfuse3` (runtime) and `libfuse3-dev` (build headers)
- **macOS** — [macFUSE](https://macfuse.github.io/)
- **Windows** — [WinFsp](https://winfsp.dev/)

Build natively on each OS (cgo cannot be cross-compiled without a target
toolchain):

```sh
make build            # -> bin/qkmount   (Linux, with libfuse3-dev)
make build TAGS=      # macOS (macFUSE) / Windows (WinFsp)
```

If Go or the FUSE headers are installed somewhere unusual, copy
`hack/env.sh.example` to `hack/env.sh`, point it at your paths, and the
`Makefile` will source it automatically. On a normal machine you can ignore it
entirely.

### Install for the current user

```sh
make install                 # binary + icon + launcher into ~/.local
PREFIX=/usr/local make install   # or system-wide
./install.sh --autostart     # also start at login
```

Then `qkmount ui` from anywhere on your `PATH`, or use the *QKMount* launcher.

## Quick start

```sh
# 1. Sign in to Google Drive with just a browser — no client ID or token needed
qkmount auth gdrive
qkmount auth gdrive --name "My Drive" --automount

# 2. Bring across existing rclone remotes (Drive, SFTP, Dropbox, ...)
qkmount import-rclone
qkmount import-rclone --dry-run        # preview only
qkmount import-rclone --overwrite      # replace same-named connections

# 3. Or add one by hand
qkmount add --name work --type sftp \
  --set host=server.example.com --set user=alice --set password=secret \
  --mountpoint ~/QKMount/work

# 4. Start the daemon + tray, then open the UI
qkmount start
qkmount ui

# 5. Mount / unmount
qkmount mount work
qkmount unmount work
```

In the web UI, choosing **Google Drive** shows a **Sign in with Google Drive**
button: click it, approve access in the browser tab that opens, and QKMount
captures the token and adds the connection automatically — the account e-mail
becomes the connection name. The same works for Dropbox and OneDrive once a
server-side app key is configured (see below).

Connections with **auto-mount** enabled are re-mounted whenever the daemon
starts and kept mounted while it runs.

## Command line

| Command | Description |
|---------|-------------|
| `qkmount serve` | Run the daemon + web UI in the foreground |
| `qkmount start` / `stop` | Start / stop the background daemon |
| `qkmount ui` | Open the web UI (starts the daemon if needed) |
| `qkmount list` | List connections and their live status |
| `qkmount mount <name>` / `unmount <name>` | Mount / unmount a connection |
| `qkmount add …` | Add a connection |
| `qkmount auth gdrive\|dropbox\|onedrive [--name N] [--automount] [--client-id ID --client-secret SEC]` | Sign in via browser and add the connection |
| `qkmount import-rclone [--dry-run] [--overwrite] [--config PATH]` | Import rclone remotes |
| `qkmount autostart on\|off` | Enable/disable start at login |
| `qkmount remove <name>` | Delete a connection |
| `qkmount version` | Print the version |

The config directory defaults to `~/.config/qkmount` and can be overridden with
`QKMOUNT_CONFIG_DIR`.

## Web UI and tray

- The UI is served at `http://127.0.0.1:8321/` (change the port/bind in
  Settings). It lists connections with live status, lets you add/edit/remove
  them, and mount/unmount with one click.
- The **tray icon** shows every connection; left-click opens the UI, and each
  connection is a one-click mount toggle.

## Where things live

| Path | Contents |
|------|----------|
| `~/.config/qkmount/config.json` | Connections and settings (mode `0600`) |
| `~/.config/qkmount/logs/` | Per-mount logs |
| `~/.config/qkmount/daemon.log` | Daemon log (when started in background) |
| `~/QKMount/<name>` | Default mount points |

## Security and credentials

- Credentials are stored in the config file, **mode 0600**, and secrets are
  redacted before being sent to the web UI. They are **not** yet stored in the
  OS keychain.
- rclone passwords are stored obfuscated by rclone; `import-rclone` reverses
  that obfuscation and re-stores the plaintext in QKMount's own config.
- Prefer read-only mode for shares you only browse.
- The web UI binds to `127.0.0.1` by default.

## Known limitations (read this)

QKMount is honest software; here is what it does *not* do well yet.

- **Google Drive uses a shared OAuth client by default.** The built-in
  **Sign in with Google Drive** flow needs no setup, but it falls back to the
  public rclone client (`projects/202264815644`), whose request quota is shared
  with every rclone user. In practice this account is limited to roughly **45
  `files.list` calls per minute**, after which every call returns
  `403 rateLimitExceeded`. A single folder browse plus a file write can exhaust
  that, so Drive is only usable for very light, intermittent work on the shared
  client. When the limit is hit a write fails with `Input/output error`, and the
  mount log (`~/.config/qkmount/logs/<conn>.log`) records the real reason:

  ```
  Quota exceeded for quota metric 'Queries' and limit 'Requests per minute'
  ... consumer 'project_number:202264815644' ... reason: rateLimitExceeded
  ```

  QKMount backs off for a short cooldown after a quota error instead of
  hammering, caches directory listings, and caches *negative* lookups so a burst
  of lookups does not burn the budget. That helps, but it does not add quota.

  **Fix — use your own OAuth client (roughly 250× the budget).** Create one in
  Google Cloud Console (*APIs & Services → Credentials → Create credentials →
  OAuth client ID → Desktop app*) and hand it to QKMount any one of these ways,
  before signing in:

  | Where | How |
  |-------|-----|
  | Web UI | Paste the client ID/secret into the Drive form, *then* click sign-in |
  | CLI | `qkmount auth gdrive --client-id ID --client-secret SEC` |
  | Environment | `QKMOUNT_GOOGLE_CLIENT_ID`, `QKMOUNT_GOOGLE_CLIENT_SECRET` |
  | Build time | `-ldflags "-X qkmount/internal/oauthflow.DefaultGoogleClientID=… -X qkmount/internal/oauthflow.DefaultGoogleClientSecret=…"` |

  Rate-limits are retried once, then a short cooldown is applied, and directory
  listings (including *negative* lookups) are cached so a burst of file-system
  lookups does not burn the quota. This is still the single biggest reliability
  caveat until you supply your own client.
- **Google Docs, Sheets, Slides and friends are exported, not read directly.**
  These Docs Editors files have no binary content of their own, so QKMount
  presents them with the matching Office extension — `Report.docx`,
  `Budget.xlsx`, `Deck.pptx`, drawings as `.png`, forms as `.zip` — and exports
  them when you read them. Measuring a document's exported size costs one small
  request per file per session (cached), and the first read downloads the
  export. Drive shortcuts and non-exportable Google types (`…/video`, `…/map`)
  are hidden so they cannot fail on open.
- **Browser sign-in for Dropbox/OneDrive needs a server-side app.** Dropbox and
  OneDrive have no shared default, so set `QKMOUNT_DROPBOX_KEY` /
  `QKMOUNT_DROPBOX_SECRET`, or `QKMOUNT_ONEDRIVE_CLIENT_ID` /
  `QKMOUNT_ONEDRIVE_CLIENT_SECRET`, to enable the sign-in button for them.
  Google Drive works out of the box.
- **Writes are staged, not random-access.** For non-native providers, writes go
  to a temporary file and are uploaded when the file is closed; opening an
  existing file without truncation re-reads and re-uploads its content. Large
  in-place edits are therefore expensive. The file size and mtime you see while
  a write is in flight come from the local staged copy.
- **Read performance varies.** S3/Azure/B2/Drive support ranged reads; **Dropbox
  does not**, so reads there fetch the whole file and slice it.
- **No offline cache / no read-ahead prefetch.** Metadata and small reads are
  cached briefly (a few seconds) per directory.
- **FUSE is required**, so a kernel driver must be installed (Linux `libfuse3`,
  macFUSE, WinFsp). On Windows, mounting requires WinFsp; the tray/UI work
  without it.
- **Host-key verification** for SFTP is currently lenient (it does not pin host
  keys). Treat first connections to unknown servers with care.
- **Rename of remote directories** works where the provider supports it
  (Drive/Dropbox/OneDrive/WebDAV/SMB/local); prefix-only stores (S3/Azure/B2)
  surface directories but cannot rename them server-side.
- The tray uses the freedesktop StatusNotifierItem protocol; on desktops
  without a StatusNotifier host it simply won't appear (the daemon still runs).

## Architecture

```
cmd/qkmount        CLI: serve/start/stop/ui/add/import-rclone/...
internal/config    JSON connection store, redaction, reload
internal/backend   Backend interface + one file per service
internal/vfs       cgofuse adapter (FUSE/macFUSE/WinFsp)
internal/mount     supervisor + auto-remount, platform helpers
internal/server    HTTP API + embedded web UI
internal/tray      StatusNotifierItem tray over DBus
internal/rclone    rclone config import + password reveal
```

A mount is a child process (`qkmount __mount <configDir> <connectionID>`); the
daemon supervises it and restarts it on unexpected exit.

## Development

```sh
make build
make test
make vet
```

`gofmt` is expected; `make fmt` runs it. If your toolchain lives off the beaten
path, copy `hack/env.sh.example` to `hack/env.sh` and edit it.

The test suite includes an in-process WebDAV server, a mock S3 server, and an
S3 Signature V4 check, so most of the object-store layer is exercised without
network access. A live Google Drive test is available and skipped unless
`QKMOUNT_TEST_GDRIVE_TOKEN` is set:

```sh
QKMOUNT_TEST_GDRIVE_TOKEN='{...token json...}' \
  go test -tags fuse3 -run TestGDriveLive ./internal/backend/ -v
```

## Contributing

Bug reports, fixes and new backends are welcome — see
[CONTRIBUTING.md](CONTRIBUTING.md). For anything security-related, please follow
[SECURITY.md](SECURITY.md) and report privately.

## License

[MIT](LICENSE).
