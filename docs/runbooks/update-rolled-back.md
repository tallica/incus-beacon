# An update rolled back or failed

## Symptom

`incus-beacon update` exits non-zero, and the instance has
`user.incus-beacon.update-status` set to `rolled-back` or `failed`, with the
cause in `user.incus-beacon.update-reason`:

```sh
incus config get <instance> user.incus-beacon.update-status
incus config get <instance> user.incus-beacon.update-reason
```

## Cause

`rolled-back`: one step of the update failed. The usual cause is that the new
image didn't start, stopped again within `--update-settle`, or wasn't `healthy`
within `--update-wait`. The instance runs the old image again, on its volumes as
they were before the update, except a volume another instance also uses: that
one keeps what the new image wrote. The ref is in
`user.incus-beacon.update-failed` and is not tried again.

`failed`: the update failed, and so did a step of the rollback, named in the
reason after `roll back`. The instance may be stopped, or on the new image.

## Fix

For `rolled-back`, look at why the new image failed before trying it again:

```sh
incus console <instance> --show-log
incus info <instance> --show-log
```

`incus console --show-log` prints only what the instance wrote since it was last
read, so a second look comes back empty. The whole console log, kept across
restarts and rebuilds, is `/var/log/incus/<instance>/console.log` on the Incus
host, readable by root (seen on Incus 7.6, installed from a package).

With an [audit log](../../README.md#audit-log), its records show the step that
failed, with its error, and each undo after it:

```sh
jq -c 'select(.instance == "<instance>") | {time, op, event, step, phase, error}' <audit-log>
```

A newer image is tried on its own. To retry the same one, after fixing the
cause, remove the failure record:

```sh
incus config unset <instance> user.incus-beacon.update-failed
incus-beacon update --instance <instance>
```

For `failed`, finish the rollback by hand from the reason. If the reason or
`incus start <instance>` names `Instance storage pool not found`, the root
volume is lost and a rebuild can't work: follow
[instance-storage-pool-not-found.md](instance-storage-pool-not-found.md)
instead. Otherwise, the old image is the fingerprint Incus still caches; the
snapshot is the newest `incus-beacon-*` one on each volume:

```sh
incus stop <instance> --force
incus rebuild <fingerprint> <instance>
incus storage volume snapshot restore <pool> <volume> <snapshot>
incus start <instance>
```

A shared volume left unrestored can be restored the same way, from its newest
`incus-beacon-*` snapshot, once every instance using it is stopped.

If an update was interrupted, `user.incus-beacon.update-claim` holds the step it
reached, the old ref and fingerprint, the snapshot name and any dropped
environment keys. The next `update` of the instance rolls it back once the claim
has expired. Remove the claim by hand only when no update runs.

## Verify

`incus list <instance>` shows it `RUNNING`, and
`incus-beacon check --instance <instance>` no longer shows an error.
