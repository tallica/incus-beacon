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
| `internal/logx`     | zerolog setup: level, JSON or console format, stderr only                                                  | zerolog                           |
| `internal/incusx`   | Connects to Incus, lists watched OCI instances as targets, looks up image sources, writes `user.beacon.*`  | Incus client, incus-compose trust |
| `internal/policy`   | Turns an instance's user-set keys into a per-instance policy                                               | stdlib                            |
| `internal/registry` | Manifest HEAD and GET, platform selection, layer fingerprint, tag listing, registry auth, per-host backoff | oras-go                           |
| `internal/semverx`  | Splits tags into version and suffix, filters candidates and picks the newest                               | Masterminds/semver                |
| `internal/check`    | The engine: turns targets into results, with a per-process cache and dedupe                                | registry, semverx, policy         |
| `internal/report`   | Table and JSON rendering of results and status rows                                                        | stdlib                            |
| `internal/notify`   | Detects status transitions and sends them through shoutrrr URLs or a webhook                               | shoutrrr                          |
| `internal/serve`    | The interval loop with jitter, `/health` and `/ready`, graceful shutdown                                   | check, incusx, notify             |

`internal/incusx` and `internal/registry` sit behind interfaces, so the
`internal/check` engine is tested against fakes and never needs a daemon or the
network.

## Key flows

### Check cycle

1. **List targets.** Fetch every OCI instance in the projects in scope (marked
   by `volatile.container.oci` or `image.type=oci`). Read keys from the expanded
   config, so profile-set policy applies too. Skip instances the policy leaves
   unwatched.
2. **Resolve the image ref** (see below) and the platform, which comes from
   `image.architecture`.
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
   1. List the tags.
   2. Keep tags that have the current tag's suffix and precision, aren't
      prereleases (unless the current tag is one), and pass the include, exclude
      and constraint filters.
   3. Report the highest tag that is above the current one.
6. **Write results.** Write status keys only where a value changed, then send a
   notification for each transition (`serve` only, or `check --write`).

### Image ref resolution

The first source that yields a ref wins:

1. `user.beacon.image`, or `user.label.beacon.image`.
2. `user.image_alias`, which incus-compose sets to the fully resolved ref.
3. The `update_source` (server and alias) of the instance's image record, looked
   up by `volatile.base_image` in the instance's project. Whether this survives
   an image copy or prune is to verify.
4. Otherwise the result is `unknown`, with the reason "no registry ref".

Refs are normalised with Docker conventions: `nginx` becomes
`docker.io/library/nginx:latest`.

### Connecting to Incus

- **As a container (`serve`):** HTTPS to `--incus`. On the first run the beacon
  redeems a one-time trust token from `--token-file`, generates a client
  certificate, and keeps it in `--state-dir`. Later runs reuse that certificate.
  This is the pattern ic-healthd, ic-dns and incus-caddy-config use.
- **From a host (CLI):** an `incus` CLI remote (`--remote`), or an explicit
  `--client-cert` and `--client-key`.

## Invariants

- **The fingerprint is the layer hash, not the manifest digest.** Incus sets
  `volatile.base_image` to a sha256 over the image's layer digests, so a
  manifest digest never matches it. The exact concatenation is to verify against
  `client/oci_images.go` in Incus, and a test vector from a real image pins it.
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
