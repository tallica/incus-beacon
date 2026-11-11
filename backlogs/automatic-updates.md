Status: **idea, designed, not started** (2026-11-07).

# Automatic updates

Source: the v0.1 design session (2026-11-04); design pass and rebuild tests
against Incus 7.4 and 7.6 (2026-11-07).

## Why

Recreating an instance on a newer image by hand means looking up the new ref,
backing up its data, rebuilding, and checking it came back healthy. incus-beacon
already knows the newer ref, so it can do the rest, with the safety steps a
manual update tends to skip.

## Scope

Only instances incus-beacon is the sole owner of: those created with
`incus launch` or `incus-beacon install`. An instance with
`user.incus-compose.managed=true` is never rebuilt, because its compose file is
the desired state and incus-beacon can't change that file:

- incus-compose keeps `user.image_alias` at the file's tag, so after a rebuild
  onto a newer tag the instance reads as
  [compose drift](../docs/runbooks/compose-drift.md) until the file changes.
- `incus-compose up --pull always` then deletes the instance and recreates it on
  the file's tag, a silent downgrade (verified with incus-compose 1.3.5).

`update=auto` on such an instance is refused with `status=error` and a reason
saying the compose file must be updated. Compose stacks are updated through the
file instead: see the "Updating compose stacks" item in
[BACKLOG.md](../BACKLOG.md).

## What

Opt-in per instance with `user.beacon.update=auto`. `notify`, the default, keeps
today's report-only behaviour.

1. Refuse unless the Incus server is 7.6.0 or later (see "Depends on").
2. Back up the custom volumes with volume snapshots or a copy. Not an instance
   snapshot: `rebuild` fails with "Cannot remove an instance volume that has
   snapshots" (verified).
3. Drop `environment.*` keys whose values equal the old image's environment
   (fetched from the registry by digest), so the new image's values apply. Keys
   with other values are user overrides and stay.
4. `POST /1.0/instances/<name>/rebuild` from the new ref, with the instance
   stopped. It keeps devices, custom volumes and `user.*` keys, sets `image.id`
   to the new tag, so the next check tracks it, and resets the root filesystem,
   even when the image is unchanged (verified).
5. Start the instance and wait for ic-healthd's `user.healthcheck.status`, or a
   timeout when the instance has no health check.
6. Roll back on failure, and report either outcome through the notifiers.
7. Respect a maintenance window, and update at most one replica of a service at
   a time.

## Needs from the maintainer

- The maintenance window format.
- Whether rollback is automatic or only reported.
- Whether [cooldown](../BACKLOG.md) lands first; it matters more once a report
  triggers an action.

## Depends on

- **Incus 7.6.0 or later on the server.** Up to 7.4, a rebuild keeps the old
  image's environment and `oci.entrypoint`, `oci.cwd`, `oci.uid` and `oci.gid`,
  so a rebuilt nginx 1.28 still reports `NGINX_VERSION=1.27.5` (verified on
  7.4). lxc/incus#4019 (in 7.5.0) stopped copying the environment into the
  config, and 7.6.0 refreshes the `oci.*` keys that still hold the old image's
  defaults, keeping user overrides (both verified on 7.6).
- **Step 3 for instances created before 7.5.** Neither fix removes environment
  keys an older Incus copied into the config. On 7.5 and later those count as
  user overrides and beat the new image's values through every rebuild (verified
  on 7.6 with a hand-set copy). On a server upgraded from an older release,
  every OCI instance created before the upgrade carries such keys. The rule
  can't tell a key the user set to the image's own value, which is harmless to
  drop.
