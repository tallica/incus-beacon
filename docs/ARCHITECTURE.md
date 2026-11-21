# Architecture

How incus-beacon is put together and the invariants the code relies on but can't
show by itself. Setup and commands are in [DEVELOPMENT.md](DEVELOPMENT.md).

## Principles

- **Report, and act only when asked.** `check` and `serve` detect newer images
  and record them, changing only their own `user.incus-beacon.*` keys. Only
  `update`, and `serve --update` for instances that set `update=auto`, stop,
  rebuild or restart an instance, one at a time, and only after the safety steps
  under [Updating](#updating). `install` and `uninstall` create and remove only
  beacon's own instance, volume and certificate.
- **Incus is the state.** The beacon keeps no database. What it last concluded
  about an instance is stored on that instance, so a restarted or replaced
  container picks up where the last one left off. Lazyincus and the `incus` CLI
  can read the result too.
- **One writer per key.** This follows the incus-compose convention, where
  ic-healthd writes only `user.healthcheck.status`. The beacon reads keys the
  user sets and writes only the keys it owns, with the exceptions listed under
  [Invariants](#invariants).
- **Cheap on registries.** A manifest HEAD comes first, and the more costly GET
  only follows when the digest moves. Replicas of the same image share one
  lookup.
- **Small and static.** Standard library first, no CGO, and one binary that
  serves as both the CLI and the daemon. Called as `incus-beacond`, its name in
  the image, it runs `serve` when given no command.

## Components

| Package             | Job                                                                                                                                       | Depends on                              |
| ------------------- | ----------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------- |
| `cmd/incus-beacon`  | Subcommand dispatch (every command in [Usage](../README.md#usage)); builds the logger and wires the packages                              | everything below                        |
| `internal/config`   | Flag and `BEACON_*` environment parsing per subcommand, defaults, validation                                                              | stdlib                                  |
| `internal/log`      | zerolog setup: level, JSON or console format, stderr only                                                                                 | zerolog                                 |
| `internal/metrics`  | Counters, gauges and histograms in the Prometheus text format                                                                             | stdlib                                  |
| `internal/incus`    | Connects to Incus, lists watched OCI instances as targets, looks up image sources, writes `user.incus-beacon.*`, follows the event stream | Incus client, incus-compose trust       |
| `internal/policy`   | Turns an instance's user-set keys into a per-instance policy                                                                              | Masterminds/semver                      |
| `internal/registry` | Manifest HEAD and GET, platform selection, layer fingerprint, tag listing, registry auth, per-host backoff, request metrics               | oras-go, metrics                        |
| `internal/semver`   | Splits tags into version and suffix, filters candidates and picks the newest                                                              | Masterminds/semver                      |
| `internal/check`    | The engine: turns targets into results, with a per-process cache and dedupe                                                               | registry, semver, policy                |
| `internal/notify`   | Picks which written changes to send and sends them to webhooks, Discord, Slack or a command                                               | metrics                                 |
| `internal/report`   | Table and JSON rendering of results and status rows                                                                                       | check, incus                            |
| `internal/update`   | Updating an instance on its newer image: claim, snapshot, rebuild, health gate, rollback; serve's updater; the calls `upgrade` shares     | incus, registry, check, policy, metrics |
| `internal/serve`    | The interval loop with jitter, targeted checks on events, `/health`, `/ready` and `/metrics`, graceful shutdown                           | check, incus, metrics                   |

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
6. **Cooldown**, only for updates where one applies (see
   [README.md](../README.md#instance-keys)):
   1. Walk the newer tags newest first, then the rebuilt current tag, and take
      the first whose image is at least the cooldown old. Each costs a HEAD, the
      manifest GETs when its digest moved, and one GET of the image config for
      its `created` time.
   2. Created times are cached by config digest for the life of the process,
      since a config never changes.
   3. A held update leaves the status `current` and writes nothing extra; the
      held tag shows only in `check` output.
7. **Write results.** Write status keys only where a value changed.
8. **Notify** about the changes that were written (see
   [Notifications](#notifications)).

### Serving

`serve` runs a cycle at start, then every `--interval` give or take 10%, so
several beacons don't hit a registry in step. A cycle lists, checks and writes,
and has at most one interval to finish. After a failed cycle the next comes
after a minute, doubling with each further failure up to the interval, so
`/ready` recovers soon after the cause is fixed. It connects to Incus lazily and
again after a failed listing, so a restarted daemon recovers without a restart
here. `/ready` reflects the last cycle; what each endpoint answers is in
[README.md](../README.md#endpoints).

With `--events`, `serve` also follows the Incus event stream and checks an
instance on its own as soon as it is created, updated or rebuilt. A rebuild
comes only as a finished "Rebuilding instance" operation, not a lifecycle event.
Events are gathered until two seconds pass without one, then checked in the same
goroutine as the cycles, so the two never write one instance at once. An updated
instance is checked only when a key the check reads has changed since its last
check (`Target.Inputs`), or its status keys differ from the ones beacon last
left there; beacon's own `user.incus-beacon.*` writes come back as
`instance-updated` events, and this is what keeps them from looping. The status
comparison catches an instance recreated from the same config, which has the
same inputs but none of the status keys. A dropped stream reconnects with
backoff from a second up to a minute; events missed meanwhile wait for the next
cycle. Targeted checks leave `/ready` alone. A certificate restricted to some
projects may open the all-projects stream and receives their events.

### Updating

`update` rebuilds one instance that a check found `update-available`, onto the
newer tag, or onto the same tag when it was rebuilt upstream. The new image's
ref is the tracked ref at the `latest` tag.

1. **Refuse** before anything changes, with the reason in `update-reason`, when:
   - the instance is managed by incus-compose and its own config's
     `user.incus-compose.features` doesn't list `image-override`, since an
     incus-compose without the override recreates it from the compose file's ref
     on its next `up`;
   - it runs incus-beacon, which `upgrade` handles;
   - the same ref already failed (`update-failed`);
   - it isn't running, since a stopped instance can't be health-checked;
   - it has instance snapshots, which `rebuild` refuses;
   - the server is older than Incus 7.6, whose rebuild keeps the old image's
     environment and entry point.
2. **Claim** the instance in `update-claim`, written only if the instance's ETag
   still matches what was read, so two processes can't both win.
3. **Run the steps**, recording in the claim which step has begun:
   1. Stop gracefully, forcing it after `--update-stop-timeout`. With a health
      check, wait for ic-healthd to report `stopped`, so no later status is a
      leftover from before.
   2. Snapshot each attached custom volume as `incus-beacon-<UTC time>`. An
      instance snapshot would block the rebuild.
   3. Drop the instance's own `environment.*` keys whose values are the new
      image's, or the old image's when its tag still names that image. An Incus
      older than 7.5 copied those values into the config at creation, and they
      would now override the new image's. Keys that match neither stay and are
      listed, so a copy from an old image whose tag has since moved shows up
      rather than going unnoticed.
   4. On an incus-compose instance, set incus-compose's image override:
      `user.incus-compose.image-override` to the new ref and `image-override-of`
      to the instance's `user.image_alias`. From then on, `up` keeps the
      instance on that image, even when it recreates it for another change. The
      keys go in before the rebuild, so an `up` during the update can't put it
      back on the file's image.
   5. Rebuild from the new ref.
   6. Start, then gate: the instance must keep running for `--update-settle`
      and, with a health check, report `healthy` within `--update-wait`.
      `unhealthy`, or a stop, fails at once.
4. **On failure, undo every step begun, newest first:** stop and restore the
   snapshots (only once the new image has run), rebuild from the old
   `volatile.base_image`, put the override keys and the dropped keys back as
   they were, and start and gate again. A volume another instance also uses is
   not restored, and the outcome says so; its snapshot is still taken, for a
   restore by hand once the other instances are stopped.
5. **Release** the claim and record the outcome in one write, with `applied` and
   `applied-image` naming the ref and image the instance is left on:
   `update-status` becomes `updated`, `rolled-back`, `failed` (the rollback
   failed too) or `refused`. After a success, beacon's snapshots beyond
   `--update-keep` per volume are pruned, outside the claim, so a slow delete
   can't outlast its lease and get the update rolled back.

A claim holds for 30 minutes plus twice `--update-wait` past its last step. A
claim older than that is taken as abandoned by a process that died. The next
`update` of that instance takes it over and runs the same undo from the step it
records, so a crash never leaves an instance stopped or on an unchecked image. A
process that finds its own claim taken over stops without touching the instance.
An update stopped before its first step changed nothing, so it reads `refused`
and the ref is not marked failed.

### Updating from serve

With `--update`, each check (cycle or targeted) hands its results to the updater
in the same goroutine, after the status writes, so a check and an update never
write one instance at once. For each result:

1. An abandoned claim (see above) is rolled back, whatever the status.
2. Otherwise the result must be `update-available` with `update=auto`, inside
   the instance's window or the daemon's, and its ref must not be the one in
   `update-failed`.
3. Once a ref rolls back or fails, the rest of that run skips it, so a broken
   release stops after its first replica.

A started update runs to its end even past the cycle's deadline or on shutdown,
since cutting it would force a rollback. No new one starts once the deadline has
passed. What bounds it is the claim's lease: each step, and each undo of a
rollback, must finish within it, and the claim is renewed before each. A hung
Incus call, such as a stalled pull, therefore fails its step before another
process could take the claim over. Each claim write, with its retries, has a
minute. A single API request can't be cut short from beacon: the Incus client
takes no context for it, and a client-wide timeout would also cut the long
operation waits and the event stream. Stopping `serve` waits for a running
update. If Incus kills the process first, the claim is left, and the next cycle
rolls the update back once the claim expires. The updated instances are then
checked again, and the same message carries the outcomes, in place of the
"update available" line of an instance that was updated.

### Notifications

The written `user.incus-beacon.*` keys are the dedupe state: a change is sent
only for an instance whose write succeeded, by comparing the keys before and
after. So a restarted beacon never repeats a message, and nothing else needs
storing. The cost is that a failed send is not retried; the next one comes with
the next change. Which changes count is in
[README.md](../README.md#notifications); recoveries stay quiet because the
update or fix that caused them was someone's own action.

Full cycles and targeted checks both notify, so `--events` reports an update
within seconds too. Every notifier gets each message in turn, with its own 30 s
timeout, in the cycle goroutine. A notifier is chosen by URL scheme, and every
URL is checked when `serve` starts, so a typo stops it then rather than at the
first update.

### Image ref resolution

Incus describes every image it pulls from a registry as
`<registry>/<repo> (OCI)` (`client/oci_images.go`, unchanged since OCI support
arrived in Incus 6.3). Any other OCI image was built or imported locally, so no
registry holds its layers and no lookup could match them: it is `unknown`, with
the reason "not pulled from a registry", unless `user.incus-beacon.image` names
a ref to check anyway. An image made with `incus publish`, or a pulled image
exported and imported again, keeps no `image.*` keys, and an instance created
from it is not an OCI instance, so it is not watched at all. Otherwise, the
first source that yields a ref wins:

1. `user.incus-beacon.image`, or `user.label.incus-beacon.image`.
2. `user.image_alias`, which incus-compose sets to the fully resolved ref. When
   the repository or tag in the image keys (item 5) differs from it, the
   instance was rebuilt onto another image by hand: it is `error` with "compose
   drift", not checked, since the compose file's ref would report a false update
   back to it. Registry hosts are not compared, as a mirror can name the same
   image differently.

   For items 1 and 2: once beacon has updated the instance, while the ref still
   names `user.incus-beacon.requested` (compared as Docker spells refs out), the
   ref tracked is `user.incus-beacon.applied` instead. So an update is not
   reported back to the ref it moved from. A ref from item 2 keeps its drift
   check, now against `applied`; one from item 1 has none, since its image keys
   may name another tag of the same build. A ref that differs from `requested`
   wins again, and the next update records it as `requested`. On an
   incus-compose instance, incus-compose's override is tracked instead, on
   incus-compose's own terms (`image-override-of` equals `user.image_alias`),
   whichever item gave the ref, and it wins over `applied`: it is what
   incus-compose will run. When incus-compose recreates an instance on its
   override, the new instance has the override keys but none of beacon's.

3. `user.incus-beacon.applied`, the ref beacon last left the instance on, while
   `applied-image` still equals `volatile.base_image`. Beacon wrote both for
   this exact image, so they beat what Incus records: a rebuild from a
   fingerprint, as every rollback is, copies the image record's `image.id`,
   which Incus 7.6 can store pinned by digest (`library/busybox@sha256:…`), and
   the record can name another tag of the same build (item 4). Without this, a
   rolled-back instance reads "no registry ref" once Incus prunes the image. The
   fingerprint check drops the record once someone rebuilds the instance by
   hand; a failed rollback leaves the record as it was, and the check then
   decides whether it still applies.
4. The `update_source` (server and alias) of the instance's image record, looked
   up once per `volatile.base_image` in the instance's project. Incus prunes
   cached images, so this record is often gone. It can also name a tag other
   than the one pulled: Incus keeps one record per build, so pulling a tag whose
   build it already holds under another reuses that record, tag and all (seen on
   Incus 7.6). Item 1 then names the tag that was asked for.
5. The image keys Incus copies onto the instance: `image.description` holds
   `<registry>/<repo> (OCI)` and `image.id` holds `<repo>:<tag>` as launched. An
   `image.id` pinned by digest has no tag to track and gives no ref.
6. `user.incus-beacon.current`, the ref the last check tracked, while
   `current-image` still equals `volatile.base_image`. Incus 7.6 can pin
   `image.id` by digest from an image's first launch, so an instance beacon
   never updated has only item 4. Once Incus prunes the image record, this keeps
   it resolvable. It ranks last where `applied` (item 3) ranks early because
   `applied` is first-hand, the ref beacon rebuilt the instance onto, while this
   is second-hand: a copy of what an earlier source said about the image, so
   every source that can still speak comes first.
7. Otherwise the result is `unknown`, with the reason "no registry ref".

Refs are normalised with Docker conventions: `nginx` becomes
`docker.io/library/nginx:latest`.

### Connecting to Incus

- **Over HTTPS (a container, or any host):** `--incus` with `--client-cert` and
  `--client-key`, a certificate the server already trusts.
  `--server-fingerprint` pins the server's self-signed certificate.
- **With a trust token (the container default):** `--token` and `--state-dir`,
  as ic-healthd does. The first run generates a certificate, redeems the token
  with it and saves the pair and the token's server fingerprint in the state
  dir; later runs use those and never touch the token. The files are staged
  before redeeming, because a token is spent once, even when Incus then refuses
  the certificate: an unwritable state dir must fail while the token is still
  good. The certificate is backdated an hour, since Incus refuses one that isn't
  valid yet by its own clock. `--token-file` reads the token only when
  enrolling, so the file may be gone afterwards.
- **Installed (`install`):** the installer plays incus-compose's part for
  ic-healthd. Over its own trusted remote it creates the state volume, a token
  named after the instance and the instance, pushes the token to
  `/run/secrets/token` so it never sits in the instance config, starts it and
  polls the trust store until a certificate of that name appears. A failure
  before the start undoes what it created; after the start the instance stays,
  because its log explains the failure. The instance is set to `track=semver`
  when its image tag is a version, since it runs a `major.minor` tag that digest
  tracking would never move past. Incus accepts a token restricted to a project
  that doesn't exist, so the projects are checked first. The name is refused
  when a certificate already has it, since `uninstall` removes certificates by
  that name, along with unredeemed tokens.
- **Upgrading the installed instance (`upgrade`):** a rebuild, not a delete and
  create, because it keeps the config `install` wrote, the state volume with the
  certificate and the notification URLs, and the trust entry, so no new token is
  needed. Readiness is the image's own `HEALTHCHECK` probe of `/ready`, run with
  `incus exec`: it fails only when a cycle can't list instances, so a registry
  outage doesn't roll back a good release. The rollback rebuilds from the old
  `volatile.base_image` fingerprint, which Incus still caches at that point, so
  it needs no registry. Verified on Incus 7.6 only; older servers keep the old
  image's `oci.*` keys through a rebuild, harmless while the image's entrypoint
  stays the same (to verify).
- **From a host (CLI):** an `incus` CLI remote (`--remote`, or the CLI's
  default), which reuses that remote's certificates or local socket.

## Invariants

- **The fingerprint is the layer hash, not the manifest digest.** Incus sets
  `volatile.base_image` to a sha256 over the layer digest strings
  (`sha256:<hex>`), concatenated with no separator (`computeFingerprint` in
  Incus `client/oci_images.go`), so a manifest digest never matches it. Incus
  picks the manifest for its own host's platform, so the beacon selects by that
  platform. A test vector read from a running instance pins this.
- **The beacon writes only `user.incus-beacon.*`, and only on change.** Every
  config write emits an `instance-updated` lifecycle event, and other watchers
  (lazyincus, ic-healthd, incus-compose) react to those. So there is no "last
  checked" key; that value lives in memory and on `/ready`. The one exception is
  the move from the pre-0.5 `user.beacon.*` names: an instance still holding
  them is written once, with the same values under the new names, so the dedupe
  state and `since` carry over rather than reading as never checked. `update`
  also removes the `environment.*` keys of step 3 under [Updating](#updating),
  sets incus-compose's two override keys of step 4, which incus-compose leaves
  to an updater, and puts both back as they were on a rollback.
- **Writes touch the instance's local config, never the expanded config.**
  Writing the expanded config would copy profile keys onto the instance. Writes
  are guarded by the instance ETag and retried once on a conflict, and wait
  about 15 seconds at most while a start or stop holds the instance, as one does
  right after a launch. `update` retries up to ten times instead, re-reading its
  claim each time, since ic-healthd writes its status while an update runs.
- **Logs go to stderr and reports to stdout,** so `check --format json` can be
  piped. Credentials, trust tokens and private keys are never logged.
- **A failed check keeps the last known `user.incus-beacon.latest`.** It sets
  `status=error` and an error reason, so a flaky registry never hides an update
  that was already found.
