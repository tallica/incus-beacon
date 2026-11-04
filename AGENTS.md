# incus-beacon

Watches OCI registries for newer images of Incus OCI containers and reports what
can be updated.

This file is for any agent (or person) working in the tree; it is the one home
for how work is done here. README.md is the front door for users. This table is
the one map of where each kind of fact lives; everywhere else links to the home:

| Fact                                        | Home                                                          | Never                                 |
| ------------------------------------------- | ------------------------------------------------------------- | ------------------------------------- |
| What it is, install, usage                  | [README.md](README.md)                                        |                                       |
| How work is done                            | AGENTS.md (this file)                                         | in CLAUDE.md or skills                |
| Commit format, types, scopes, branches, PRs | [CONTRIBUTING.md](CONTRIBUTING.md)                            |                                       |
| What changed for users                      | [CHANGELOG.md](CHANGELOG.md)                                  | as a dump of the git log              |
| Unscheduled and rejected ideas              | [BACKLOG.md](BACKLOG.md), [backlogs/](backlogs/)              | in code comments                      |
| Why the code is shaped this way             | [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md), commit messages | beyond a one-line comment at the site |
| Dev setup, make targets, testing            | [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md)                    |                                       |
| Release procedure                           | [docs/RELEASING.md](docs/RELEASING.md)                        |                                       |
| Failure modes and fixes                     | [docs/runbooks/](docs/runbooks/)                              |                                       |
| Reporting vulnerabilities, audit status     | [SECURITY.md](SECURITY.md)                                    |                                       |
| How docs are written                        | [docs/README.md](docs/README.md#writing)                      |                                       |
| Template version and how it was rendered    | [.template.json](.template.json)                              | edited by hand                        |
| Current session state                       | `sessions/` (local only)                                      | committed                             |

## Status

Pre-1.0 and not yet released: `check`, `status` and `serve` work against a real
Incus; trust tokens, notifications and automatic updates are on the backlog. Go
1.27, built static with `CGO_ENABLED=0`. No dependency is pinned below its
latest release.

## Working on this

- **Committing is the maintainer's call.** Make the change, run the checks, say
  what happened and what couldn't be verified. A commit waits to be asked for.
  Pushing, merging, opening a PR and publishing anything each need their own
  explicit go-ahead; being asked once is not standing permission, and "commit"
  never implies "push".
- **Every commit is ready to publish.** Treat a commit as public the moment it
  is made: history is hard to rewrite and forks keep it. Nothing goes in that is
  a secret, personal data (home paths, real IPs, MACs, hostnames, device IDs,
  Wi-Fi names), an unscrubbed log or capture, copyrighted material, or a local
  artifact. Use placeholders: `192.168.1.10`, `<device-ip>`, `/path/to/data`,
  `example.com`. When in doubt, leave it out and ask.
- **Stage by path, never `git add -A` or `git add .`.** Stage in one command and
  commit only the index in the next (no `-a` or paths on `git commit`), so the
  staged diff is what gets reviewed.
- **`make check` and `make fmt-check` pass before every commit.** Local checks
  are the gate. After a push, one quick CI status check is fine; don't sit
  polling it.
- **CHANGELOG.md is part of the change, not a follow-up.** Anything a user would
  notice gets an entry in the same commit, placed and written as the
  CHANGELOG.md header describes.
- **One commit per logical change.** Never bundle unrelated fixes; split shared
  CHANGELOG and doc hunks between their commits too.
- **One home per fact.** Each fact has an owning doc (map above); everywhere
  else links to it. A fact in two places goes stale in one of them. Docs that a
  change makes wrong are fixed in the same commit. When a section moves, fix the
  links into it.
- **Comments earn their line.** One line, unless it carries a constraint the
  code can't show (an external quirk, timing, a protocol rule). Around 12%
  comment lines reads right; more reads as bloat. Never narrate what the next
  line does.
- **Unverified claims say so.** Behaviour not yet confirmed against the real
  system is marked "to verify" in docs until it is.
- **Licence headers are not touched.** Never remove or alter one; check LICENSE
  (and THIRD_PARTY_NOTICES, where there is one) and ask instead.
- **Settled decisions stay settled.** A rejected idea is recorded with its
  reason where the map above keeps ideas; don't pitch it again without new
  information.
- **A refusal is final.** If a guard, hook or permission prompt refuses a
  command, don't rephrase it, split it or route it through another tool. Stop
  and ask.
- **Write for a stranger.** No first person, no "on my machine", no private
  context in comments, docs or commit messages.

## Before committing

Every agent checks this by hand, whatever its tooling enforces:

1. `make check` and `make fmt-check` are green.
2. The staged diff (`git diff --cached`) has nothing from the publish-safety
   list above.
3. Docs are wrapped prose with relative links, and describe the current state
   rather than its history.
4. Re-read every comment the change adds; cut the ones that restate the code.
5. User-visible changes have their CHANGELOG line; docs the change made wrong
   are fixed; a backlog item it finishes is deleted.
6. Grep the change's key phrases across every touched file and the commit
   message: a rationale belongs in one place.

## Commits and branches

Message format, types, scopes, branch flow and pull requests are in
[CONTRIBUTING.md](CONTRIBUTING.md#commits).

- End with a trailer naming the agent that did the work, e.g.
  `Co-Authored-By: <agent/model> <noreply@example.com>`.
- A feature branch stays unmerged until the maintainer says to merge.
- Worktrees live next to the repo: `../incus-beacon.<branch>`.
- Before reporting ahead/behind or unpushed counts, `git fetch -q` first: the
  maintainer pushes between sessions.

## Sessions and handoff

- End every piece of work with: the files changed and what each change does,
  anything not verified, and unpushed commits by short hash and subject.
- If work stops midway, say exactly what ran, what didn't, and what state things
  are left in.
- Handoffs for the next session go in `sessions/YYYY-MM-DD_<topic>.md` (local
  only, never committed). Write a new dated file each time; link earlier ones
  rather than repeating them.

Start a handoff from [docs/templates/handoff.md](docs/templates/handoff.md).

## Building and testing

```sh
make check      # the gate before every commit, with make fmt-check
make fmt        # fix what make fmt-check reports
make test
make lint
```

Details live in [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md).

Releases follow [docs/RELEASING.md](docs/RELEASING.md).

## Go

- Standard library first; a new dependency needs a reason, a licence on the list
  in `scripts/notices.sh` and a `make notices` run in the same commit.
- No CGO: release binaries are static and cross-compiled.
- `context.Context` is the first parameter of anything that does I/O or can
  block.
- `cmd/` only wires things together; logic lives in `internal/`.
- Packages are named for what they hold (`internal/log`, `internal/incus`), with
  no `x` suffix; a library import that clashes gets an alias where it is
  imported.
- Golden files live in each package's `testdata/`; regenerate them with the
  test's `-update` flag, never by hand.
- `make race` when a change touches goroutines, timers or shared state.
