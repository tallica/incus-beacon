# Development

Setting up, building and testing incus-beacon. Design is in
[ARCHITECTURE.md](ARCHITECTURE.md); working rules are in
[AGENTS.md](../AGENTS.md).

## Prerequisites

`git`, `make`, Go 1.27, golangci-lint for linting and Go formatting, and Node.js
for `npx`, which runs Prettier.

## Layout

```
.
├── cmd/incus-beacon/   entry point: subcommand dispatch and wiring only
├── internal/           all logic, one package per component
│   └── version/        build version, set through ldflags
├── backlogs/           ideas too big for a BACKLOG.md line
├── docs/               topic docs, runbooks, templates
└── scripts/            helper scripts
```

The `internal/` packages and their jobs are listed in
[ARCHITECTURE.md](ARCHITECTURE.md#components).

## Make targets

| Target                          | What it does                                                      |
| ------------------------------- | ----------------------------------------------------------------- |
| `make check`                    | With `make fmt-check`, the gate before every commit; CI runs both |
| `make lint`                     | Linters                                                           |
| `make test`                     | Tests                                                             |
| `make fmt`                      | Format Go and Markdown; Markdown wraps at 80 columns              |
| `make fmt-check`                | Fails if Prettier would change anything; `make lint` checks Go    |
| `make release-notes TAG=vX.Y.Z` | Print a version's CHANGELOG section                               |

## Testing

`make test` needs no Incus daemon or network. Incus access is tested against a
fake `/1.0` API in `internal/incus/fake_test.go`; table and JSON output are
golden files in `internal/report/testdata/`, rewritten with
`go test ./internal/report -update`.

Against a real Incus, `status` and `check` without `--write` only read, so they
are safe on any remote:

```sh
make build
bin/incus-beacon status --remote <remote>
bin/incus-beacon check --remote <remote> --format json --log-level debug
```

`serve --watch opt-in` connects, lists and serves `/health` and `/ready` but
watches nothing until an instance sets `user.beacon.enable=true`, so it is a
safe way to try the daemon on a real remote:

```sh
bin/incus-beacon serve --remote <remote> --watch opt-in --interval 1m --listen 127.0.0.1:9153
curl 127.0.0.1:9153/ready
```

`check --write` changes `user.beacon.*` keys on instances, and each change emits
an `instance-updated` event. Try it on a throwaway project first:

```sh
incus project create beacon-test
incus launch docker:nginx:1.27 web --project beacon-test
incus config set web user.beacon.track=semver --project beacon-test
bin/incus-beacon check --remote <remote> --project beacon-test --write
incus config show web --project beacon-test | grep user.beacon
```

## Go targets

`make release-snapshot` also needs GoReleaser, and `make image` Docker (or
`make image CONTAINER=podman`).

| Target                  | What it does                                    |
| ----------------------- | ----------------------------------------------- |
| `make run`              | Run from source with version stamping           |
| `make build`            | Static binary in `bin/`                         |
| `make install`          | Version-stamped binary into `$GOBIN`            |
| `make race`             | Tests with the race detector                    |
| `make audit`            | govulncheck: vulnerabilities the code can reach |
| `make notices`          | THIRD_PARTY_NOTICES.md from the linked modules  |
| `make notices-check`    | Fails if THIRD_PARTY_NOTICES.md is stale        |
| `make release-snapshot` | Local GoReleaser dry run into `dist/`           |
| `make image`            | Container image for the host's platform         |
