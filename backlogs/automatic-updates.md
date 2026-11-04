Status: **idea, not started** (2026-11-04).

# Automatic updates

Source: the v0.1 design session (2026-11-04). v0.1 only reports; the long-term
aim is a watchtower-style update for Incus OCI instances.

## Why

Recreating an instance on a newer image by hand means looking up the new ref,
backing up its data, rebuilding, and checking it came back healthy. incus-beacon
already knows the newer ref, so it can do the rest, with the safety steps a
manual update tends to skip.

## What

Opt-in per instance with `user.beacon.update=auto`. `notify`, the default, keeps
today's report-only behaviour.

1. Back up the custom volumes: a snapshot, or the incus-compose backup project
   (`<stack>-backup`) for compose stacks.
2. Stop the instance.
3. Rebuild from the new ref: `POST /1.0/instances/<name>/rebuild` keeps config,
   devices and custom volumes. It needs the instance stopped and without
   snapshots, so snapshot-based backups must live outside the instance (to
   verify). Compose stacks use
   `incus-compose up --recreate --pull always --no-deps <service>` instead, so
   incus-compose stays the owner of what it created.
4. Start the instance and wait for ic-healthd's `user.healthcheck.status`, or a
   timeout when the instance has no health check.
5. Roll back to the backup on failure, and report either outcome through the
   notifiers.
6. Respect a maintenance window, and update at most one replica of a service at
   a time.

## Needs from the maintainer

- Whether to shell out to `incus-compose` or call its packages.
- The maintenance window format.
- Whether rollback is automatic or only reported.

## Depends on

- v0.1 detection and status keys.
- Whether `oci.*` overrides and the image environment are re-derived on rebuild
  (to verify).
