# Security policy

## Reporting a vulnerability

Please report security issues **privately**, not in a public issue. Use GitHub's
[private vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability)
on this repository, or contact the maintainers directly if that is unavailable.

Please include a description, the affected version/commit, and reproduction
steps if you can. We will acknowledge the report as quickly as we can.

## Handling of credentials

QKMount stores connection credentials (OAuth tokens, passwords, private keys)
in plain text in `~/.config/qkmount/config.json`, created with mode `0600` and
readable only by your user. Secrets are redacted before being sent to the web
UI, and the UI binds to `127.0.0.1` by default.

Notable limitations, by design for now:

- Credentials are **not** yet stored in the operating system keychain.
- SFTP host-key verification is currently lenient and does not pin host keys.
- The built-in web UI has no authentication; it listens on loopback only. Do not
  expose it to an untrusted network.

Run QKMount only on a machine you trust, and prefer read-only mounts for shares
you only browse.
