# Development

Setting up, building and testing incus-beacon. Design is in
[ARCHITECTURE.md](ARCHITECTURE.md); working rules are in
[AGENTS.md](../AGENTS.md).

## Prerequisites

`git`, `make`, and Node.js for `npx`, which runs Prettier.

<!-- Add the project's own tools and the versions needed. -->

## Layout

```
.
├── docs/        topic docs, runbooks, templates
├── scripts/     helper scripts
└── ...
```

## Make targets

| Target                          | What it does                                                      |
| ------------------------------- | ----------------------------------------------------------------- |
| `make check`                    | With `make fmt-check`, the gate before every commit; CI runs both |
| `make lint`                     | Linters                                                           |
| `make test`                     | Tests                                                             |
| `make fmt`                      | Format the tree with Prettier; Markdown wraps at 80 columns       |
| `make fmt-check`                | What CI checks: fails if `make fmt` would change anything         |
| `make release-notes TAG=vX.Y.Z` | Print a version's CHANGELOG section                               |

## Testing

<!-- What is unit-tested, what needs real hardware or services, and
the manual checklist to run before shipping. -->

## Go targets

These and `make check` need Go and golangci-lint; `make release-snapshot` also
needs GoReleaser.

| Target                  | What it does                                    |
| ----------------------- | ----------------------------------------------- |
| `make run`              | Run from source with version stamping           |
| `make build`            | Static binary in `bin/`                         |
| `make install`          | Version-stamped binary into `$GOBIN`            |
| `make race`             | Tests with the race detector                    |
| `make audit`            | govulncheck: vulnerabilities the code can reach |
| `make release-snapshot` | Local GoReleaser dry run into `dist/`           |
