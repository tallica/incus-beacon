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
they were before the update. The ref is in `user.incus-beacon.update-failed` and
is not tried again.

`failed`: the update failed, and so did a step of the rollback, named in the
reason after `roll back`. The instance may be stopped, or on the new image.

## Fix

For `rolled-back`, look at why the new image failed before trying it again:

```sh
incus console <instance> --show-log
incus info <instance> --show-log
```

A newer image is tried on its own. To retry the same one, after fixing the
cause, remove the failure record:

```sh
incus config unset <instance> user.incus-beacon.update-failed
incus-beacon update --instance <instance>
```

For `failed`, finish the rollback by hand from the reason. The old image is the
fingerprint Incus still caches; the snapshot is the newest `incus-beacon-*` one
on each volume:

```sh
incus stop <instance> --force
incus rebuild <fingerprint> <instance>
incus storage volume snapshot restore <pool> <volume> <snapshot>
incus start <instance>
```

If an update was interrupted, `user.incus-beacon.update-claim` holds the step it
reached, the old fingerprint, the snapshot name and any dropped environment
keys. The next `update` of the instance rolls it back once the claim has
expired. Remove the claim by hand only when no update runs.

## Verify

`incus list <instance>` shows it `RUNNING`, and
`incus-beacon check --instance <instance>` no longer shows an error.
