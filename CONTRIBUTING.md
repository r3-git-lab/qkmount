# Contributing to QKMount

Thanks for taking a look. Bug reports, fixes and new backends are all welcome.

## Building

You need Go and the platform FUSE headers:

- **Linux** — `libfuse3-dev` (Debian/Ubuntu: `sudo apt install libfuse3-dev`)
- **macOS** — [macFUSE](https://macfuse.github.io/)
- **Windows** — [WinFsp](https://winfsp.dev/)

Build natively on each OS (cgo links against the platform FUSE library, so
cross-compiling needs a full target toolchain):

```sh
make build            # Linux
make build TAGS=      # macOS / Windows
make test
make vet
```

If your Go or FUSE headers live off the beaten path, copy
`hack/env.sh.example` to `hack/env.sh` and edit the paths — the Makefile picks
it up automatically.

## Before you send a change

```sh
gofmt -w ./cmd ./internal
make vet
make test
```

Please keep changes focused and add a test where it makes sense. The suite runs
entirely offline (in-process WebDAV server, mock S3, SigV4 checks), so new
backends can usually be tested without real credentials.

## Adding a backend

Each service lives in one file under `internal/backend/` and registers itself in
an `init()` via `Register("name", ...)`. Implement either:

- the **`ObjectStore`** interface (key/value services such as S3, Azure, B2,
  Drive, Dropbox) and wrap it with `NewObjectFS`, or
- the **`Backend`** interface directly for filesystem-style services (SFTP, FTP,
  WebDAV, SMB, local).

Add the type to the table in `README.md` and a minimal constructor test so it
shows up in CI.

## Secrets

Never commit credentials, tokens, logs or a personal `config.json`. The
`.gitignore` already excludes the usual offenders.
