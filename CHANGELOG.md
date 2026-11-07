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

### Added

- `install` now makes the instance track `semver` when its image has a version
  tag, so it reports newer releases of itself; `--track digest` keeps the old
  behaviour.

## [0.4.0] - 2026-11-07

### Added

- `serve` answers `/metrics` for Prometheus: instances by status, cycle results
  and timing, status writes, and registry requests and latency by host.
- `serve --notify-url` sends a message to a webhook, Discord or Slack when an
  instance gets an update; `--notify-command` runs any program instead.
- `install --notify-url` sets up notifications, keeping the URLs in the state
  volume rather than the instance config.

### Fixed

- A mistyped or private image with no registry credentials now reads "not found,
  or private: no registry credentials", not "registry auth failed".

## [0.3.0] - 2026-11-07

### Added

- `serve --events` checks an instance within seconds of it being created,
  changed or rebuilt, instead of waiting for the next interval;
  `install --events` sets it up.
- `install --remote <remote>:<project>` puts the instance in that project
  instead of the remote's current one; `uninstall` finds the instance in any
  project.

## [0.2.0] - 2026-11-07

### Added

- `incus-beacon install` sets up `serve` on Incus in one command, joining with a
  trust token it creates; `uninstall` removes it again.
- `--token-file` reads the trust token from a file, keeping it out of the
  instance config.
- `serve` logs its version, commit and build date when it starts.

### Fixed

- `incus-beacon version` in the container image shows the commit date, as the
  release binaries do, instead of an empty one.

## [0.1.2] - 2026-11-07

### Added

- `--token` joins Incus with a one-time trust token and keeps the certificate in
  `--state-dir`; the container needs no hand-made certificate or fingerprint.

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

[Unreleased]: https://github.com/tallica/incus-beacon/compare/v0.4.0...master
[0.4.0]: https://github.com/tallica/incus-beacon/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/tallica/incus-beacon/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/tallica/incus-beacon/compare/v0.1.2...v0.2.0
[0.1.2]: https://github.com/tallica/incus-beacon/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/tallica/incus-beacon/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/tallica/incus-beacon/releases/tag/v0.1.0
