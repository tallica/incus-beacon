# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
adheres to [Semantic Versioning](https://semver.org/).

Commit types map to sections as described in
[CONTRIBUTING.md](CONTRIBUTING.md#commits).

Entries are curated, not a dump of the git log: one line each, leading with what
the user sees or does differently, under 25 words. Internals and refactors don't
get an entry.

## [Unreleased]

## [0.1.1] - 2026-11-05

### Added

- `--log-color` (`auto`, `never`, `always`) controls coloured console logs; the
  container image sets `never`, so `incus console --show-log` reads cleanly.

### Fixed

- The container image carries CA certificates, so registry lookups no longer
  fail with `certificate signed by unknown authority`.

## [0.1.0] - 2026-11-05

### Added

- `incus-beacon` takes `check`, `status`, `serve` and `version` commands; every
  flag can also be set through a `BEACON_*` environment variable.
- `incus-beacon status` lists the watched Incus OCI instances, their image and
  the update status recorded on them, as a table or JSON.
- `incus-beacon check` finds newer images for Incus OCI instances: a rebuilt
  tag, or with `track=semver` a newer version; `--write` records the result.
- Instance keys `user.beacon.enable`, `image`, `track`, `constraint`, `include`
  and `exclude` set what is watched and how, per instance or profile.
- Images built or imported locally show as `unknown` rather than being compared
  with a registry they never came from; `user.beacon.image` overrides this.
- `incus-beacon serve` checks on an interval (`--interval`, default 6h) and
  records results; `/health` and `/ready` on `--listen` report on it.
- After a failed cycle, `serve` retries after about a minute, backing off to
  `--interval`, so `/ready` recovers without waiting hours.
- A container image, `ghcr.io/tallica/incus-beacon` for amd64 and arm64, is
  published with each release; it runs `serve` as an unprivileged user.
- Release archives carry THIRD_PARTY_NOTICES.md with the licence and NOTICE text
  of every module the binary links, not only direct dependencies.

[Unreleased]: https://github.com/tallica/incus-beacon/compare/v0.1.1...master
[0.1.1]: https://github.com/tallica/incus-beacon/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/tallica/incus-beacon/releases/tag/v0.1.0
