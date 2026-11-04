# Security

<!-- prettier-ignore -->
> [!WARNING]
> **AI-assisted project**<br>
> This codebase was built with [Claude Code](https://claude.com/claude-code). It
> works for the author's specific setup but has not been independently audited.
> Review the code before running it in any security-sensitive or production
> environment.

incus-beacon holds a client certificate the Incus server trusts, and with
`--write` or `serve` it changes instance configuration. Give it a certificate
restricted to the projects it watches, and pin the server with
`--server-fingerprint`, as in [Run it on Incus](README.md#run-it-on-incus).

## Reporting a vulnerability

Report it privately through
[GitHub's private vulnerability reporting](https://github.com/tallica/incus-beacon/security/advisories/new),
not in a public issue. Include what you found, how to reproduce it, and the
version (`incus-beacon version`). Expect an answer within a week; there is no
bounty.

## Supported versions

Only the latest release gets fixes while the project is pre-1.0.
