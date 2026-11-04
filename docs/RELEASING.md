# Releasing

How a version of incus-beacon is cut. CHANGELOG.md is the source of truth: the
release notes are its section for the tag, and `make release-notes` refuses a
tag without one, so a release never goes out with empty notes.

## When

Suggest a release once `## [Unreleased]` has built up enough to be worth a
version, not after every fix.

## Version

[SemVer](https://semver.org/). Choose the bump from `[Unreleased]`:

- **major**: a breaking change, a new required setting, a removed flag or API
  (pre-1.0, this is a minor bump instead).
- **minor**: new features, flags or settings.
- **patch**: fixes only.

## Steps

1. `git fetch -q` and confirm `master` is clean and up to date with its remote.
2. `make check` passes.
3. In CHANGELOG.md, rename `## [Unreleased]` to `## [X.Y.Z] - YYYY-MM-DD`, add a
   fresh empty `## [Unreleased]` above it, and tidy the entries (merge
   duplicates, rewrite lines a later change superseded).
4. Update the links at the bottom:
   ```
   [Unreleased]: https://github.com/tallica/incus-beacon/compare/vX.Y.Z...master
   [X.Y.Z]: https://github.com/tallica/incus-beacon/compare/vPREV...vX.Y.Z
   ```
   For the first release, `[X.Y.Z]` points at
   `https://github.com/tallica/incus-beacon/releases/tag/vX.Y.Z`.
5. Check the notes: `make release-notes TAG=vX.Y.Z`.
6. Commit only CHANGELOG.md: `chore(release): vX.Y.Z`.
7. Tag: `git tag -a vX.Y.Z -m "vX.Y.Z"`.
8. **Stop and ask** before pushing: `git push && git push origin vX.Y.Z`.

Pushing the tag publishes the release: CI runs the checks on the tagged commit
and, only if they all pass, GoReleaser creates the GitHub release with these
notes.

## Downstream

The tag also runs `.github/workflows/image.yml`, which pushes
`ghcr.io/tallica/incus-beacon` for linux/amd64 and linux/arm64, tagged with the
version and its `major.minor`. It runs apart from the release job, so check it
passed. The first push creates the package as private on ghcr.io; make it public
in the package settings once (to verify on the first release).
