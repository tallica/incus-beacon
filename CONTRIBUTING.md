# Contributing

How changes get into incus-beacon. The working rules for agents and people alike
are in [AGENTS.md](AGENTS.md); this file owns the commit, branch and PR formats.

## Checks

`make check` and `make fmt-check` are the gate before every commit
([make targets](docs/DEVELOPMENT.md#make-targets)). The full pre-commit list is
in [AGENTS.md](AGENTS.md#before-committing).

## Commits

[Conventional Commits 1.0.0](https://www.conventionalcommits.org/), checked by
commitlint ([commitlint.config.mjs](commitlint.config.mjs)).

```
<type>(<scope>): <lowercase imperative subject>

Why the change was needed and what it affects, in one or two short
paragraphs.

Co-Authored-By: ...
```

- Subject: lowercase, imperative, plain English, no trailing period:
  `fix(api): stop dropping the last page of results`.
- Body: why and what it affects, not how it works. Omit it for trivial commits.

| Type       | Use for                    | CHANGELOG section         |
| ---------- | -------------------------- | ------------------------- |
| `feat`     | New user-visible behaviour | Added                     |
| `fix`      | A bug fix                  | Fixed                     |
| `perf`     | Faster, same behaviour     | Changed                   |
| `refactor` | Internal restructuring     | none                      |
| `docs`     | Documentation only         | none                      |
| `test`     | Tests only                 | none                      |
| `build`    | Build system, dependencies | none, unless users notice |
| `ci`       | CI configuration           | none                      |
| `chore`    | Anything else              | none                      |
| `revert`   | Reverting a commit         | as the reverted change    |

A breaking change adds `!` after the type (`feat(api)!: …`) and a
`BREAKING CHANGE:` footer; its CHANGELOG line is marked **Breaking**. Removals
go under Removed, security fixes under Security.

Scopes name the area touched (a package, component or doc). Keep the list short
and consistent:

| Scope | Area |
| ----- | ---- |
|       |      |

## Branches

- `master` is the default branch and always works.
- Small fixes are committed on `master` directly.
- Multi-step work goes on `feat/<topic>` (or `fix/<topic>`) and is merged with a
  merge commit, not squashed, so the history of each step survives.

## Pull requests

- Title: in commit format (`feat(api): …`); CI lints it with the commits.
- Body follows [the PR template](.github/pull_request_template.md): what changed
  for users, how it works, and a test plan whose unchecked boxes say plainly
  what wasn't verified.
- A PR with a user-facing change carries the `user-facing` label and a CHANGELOG
  entry; once labelled, CI fails it without the entry.
