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

- `incus-beacon` takes `check`, `status`, `serve` and `version` commands; every
  flag can also be set through a `BEACON_*` environment variable.

[Unreleased]: https://github.com/tallica/incus-beacon/commits/master
