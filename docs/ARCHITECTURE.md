# Architecture

How incus-beacon is put together and the invariants the code relies on but can't
show by itself. Setup and commands are in [DEVELOPMENT.md](DEVELOPMENT.md).

This is the v0.1 design. None of the components below are implemented yet. Each
lands in its own commit, and this doc changes with it where the code differs.

## Principles

- **Report, don't act.** v0.1 only detects newer images and records them. It
  never stops, rebuilds or recreates an instance. The only thing it changes on
  Incus is its own `user.beacon.*` keys.
- **Incus is the state.** The beacon keeps no database. What it last concluded
  about an instance is stored on that instance, so a restarted or replaced
  container picks up where the last one left off. Lazyincus and the `incus` CLI
  can read the result too.
- **One writer per key.** This follows the incus-compose convention, where
  ic-healthd writes only `user.healthcheck.status`. The beacon reads keys the
  user sets and writes only the keys it owns.
- **Cheap on registries.** A manifest HEAD comes first, and the more costly GET
  only follows when the digest moves. Replicas of the same image share one
  lookup.
- **Small and static.** Standard library first, no CGO, and one binary that
  serves as both the CLI and the daemon.

## Components

| Package             | Job                                                                                                        | Depends on                        |
| ------------------- | ---------------------------------------------------------------------------------------------------------- | --------------------------------- |
| `cmd/incus-beacon`  | Subcommand dispatch (`check`, `status`, `serve`, `version`); builds the logger and wires the packages      | everything below                  |
| `internal/config`   | Flag and `BEACON_*` environment parsing per subcommand, defaults, validation                               | stdlib                            |
| `internal/log`      | zerolog setup: level, JSON or console format, stderr only                                                  | zerolog                           |
| `internal/incus`    | Connects to Incus, lists watched OCI instances as targets, looks up image sources, writes `user.beacon.*`  | Incus client, incus-compose trust |
| `internal/policy`   | Turns an instance's user-set keys into a per-instance policy                                               | stdlib                            |
| `internal/registry` | Manifest HEAD and GET, platform selection, layer fingerprint, tag listing, registry auth, per-host backoff | oras-go                           |
| `internal/semver`   | Splits tags into version and suffix, filters candidates and picks the newest                               | Masterminds/semver                |
| `internal/check`    | The engine: turns targets into results, with a per-process cache and dedupe                                | registry, semver, policy          |
| `internal/report`   | Table and JSON rendering of results and status rows                                                        | stdlib                            |
| `internal/serve`    | The interval loop with jitter, `/health` and `/ready`, graceful shutdown                                   | check, incus                      |

`internal/incus` and `internal/registry` sit behind interfaces, so the
`internal/check` engine is tested against fakes and never needs a daemon or the
network.

## Key flows

### Check cycle

1. **List targets.** Fetch every OCI instance in the projects in scope (marked
   by `volatile.container.oci` or `image.type=oci`). Read keys from the expanded
   config, so profile-set policy applies too. Skip instances the policy leaves
   unwatched.
2. **Resolve the image ref** (see below) and the platform, which comes from
   `image.architecture`, or from the instance's architecture when the image
   carries none.
3. **Dedupe** targets by `(ref, platform)`. At most `--concurrency` lookups run
   at once.
4. **Digest check**, for every target:
   1. HEAD the tag.
   2. If the digest matches the cached one, reuse the cached fingerprint.
   3. Otherwise GET the manifest, select the platform from an index, and compute
      the layer fingerprint.
   4. A fingerprint different from `volatile.base_image` means the tag was
      republished.
5. **Semver check**, only for targets with `track=semver`:
   1. List the tags, once per repository per cycle.
   2. Keep tags with the current tag's shape (see
      [README.md](../README.md#instance-keys)): the same `v` prefix, number of
      numeric parts and whole suffix, compared as a unit, so a prerelease such
      as `-rc1` is never offered for a release tag.
   3. Apply the include, exclude and constraint filters, and report the highest
      tag above the current one. A newer tag outranks a rebuilt current tag.
   4. A current tag that isn't a version (`latest`) is an error under
      `track=semver`.
6. **Write results.** Write status keys only where a value changed.
   Notifications on these changes are on the [backlog](../BACKLOG.md).

### Serving

`serve` runs a cycle at start, then every `--interval` give or take 10%, so
several beacons don't hit a registry in step. A cycle lists, checks and writes,
and has at most one interval to finish. It connects to Incus lazily and again
after a failed listing, so a restarted daemon recovers without a restart here.
`/ready` reflects the last cycle; what each endpoint answers is in
[README.md](../README.md#endpoints).

### Image ref resolution

Incus describes every image it pulls from a registry as
`<registry>/<repo> (OCI)` (`client/oci_images.go`; older servers to verify). Any
other OCI image was built, imported or published locally, so no registry holds
its layers and no lookup could match them: it is `unknown`, with the reason "not
pulled from a registry", unless `user.beacon.image` names a ref to check anyway.
Otherwise, the first source that yields a ref wins:

1. `user.beacon.image`, or `user.label.beacon.image`.
2. `user.image_alias`, which incus-compose sets to the fully resolved ref.
3. The `update_source` (server and alias) of the instance's image record, looked
   up once per `volatile.base_image` in the instance's project. Incus prunes
   cached images, so this record is often gone.
4. The image keys Incus copies onto the instance: `image.description` holds
   `<registry>/<repo> (OCI)` and `image.id` holds `<repo>:<tag>` as launched. An
   `image.id` pinned by digest has no tag to track and gives no ref.
5. Otherwise the result is `unknown`, with the reason "no registry ref".

Refs are normalised with Docker conventions: `nginx` becomes
`docker.io/library/nginx:latest`.

### Connecting to Incus

- **Over HTTPS (a container, or any host):** `--incus` with `--client-cert` and
  `--client-key`, a certificate the server already trusts.
  `--server-fingerprint` pins the server's self-signed certificate. Joining with
  a one-time trust token instead, as ic-healthd does, is on the
  [backlog](../BACKLOG.md).
- **From a host (CLI):** an `incus` CLI remote (`--remote`, or the CLI's
  default), which reuses that remote's certificates or local socket.

## Invariants

- **The fingerprint is the layer hash, not the manifest digest.** Incus sets
  `volatile.base_image` to a sha256 over the layer digest strings
  (`sha256:<hex>`), concatenated with no separator (`computeFingerprint` in
  Incus `client/oci_images.go`), so a manifest digest never matches it. Incus
  picks the manifest for its own host's platform, so the beacon selects by that
  platform. A test vector read from a running instance pins this.
- **The beacon writes only `user.beacon.*`, and only on change.** Every config
  write emits an `instance-updated` lifecycle event, and other watchers
  (lazyincus, ic-healthd, incus-compose) react to those. So there is no "last
  checked" key; that value lives in memory and on `/ready`.
- **Writes touch the instance's local config, never the expanded config.**
  Writing the expanded config would copy profile keys onto the instance. Writes
  are guarded by the instance ETag and retried once on a conflict.
- **Logs go to stderr and reports to stdout,** so `check --format json` can be
  piped. Credentials, trust tokens and private keys are never logged.
- **A failed check keeps the last known `user.beacon.latest`.** It sets
  `status=error` and an error reason, so a flaky registry never hides an update
  that was already found.
