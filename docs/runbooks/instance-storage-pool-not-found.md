# An update failed and the instance has no root volume

## Symptom

`user.incus-beacon.update-status` is `failed`, and the reason names both
rebuilds:

```text
Failed rebuilding instance from image: Volume path ".../containers/<instance>" already exists
Failed getting instance storage pool name: ... Instance storage pool not found
```

`incus start <instance>` and `incus rebuild <fingerprint> <instance>` fail with
`Instance storage pool not found`.

## Cause

A config write raced the forward rebuild and recreated the root volume's
directory after the rebuild deleted the volume, so the rebuild could not create
it again. The volume's database record is gone, so no rebuild of the instance
can work. The details are in
[ARCHITECTURE.md, Updating, item 4](../ARCHITECTURE.md#updating). The new image
never ran, so custom volumes hold what they held before the update and need no
restore.

## Fix

Recreate the instance on the old image. Save its config and the old fingerprint
first; the failed rebuild left `volatile.base_image` on it, and Incus still
caches that image:

```sh
incus config show <instance> > <instance>.yaml
fp=$(incus config get <instance> volatile.base_image)
incus delete --force <instance>
```

With no root volume to remove, this delete keeps every custom volume, including
disks attached with `dependent=true`; a normal delete removes those with the
instance (both seen on Incus 7.6). It leaves the volume's empty directory
behind, owned by root, where the check that failed the rebuild would refuse the
new instance's volume. On the Incus host, check that it is empty, then remove
it:

```sh
ls -A /var/lib/incus/storage-pools/<pool>/containers/<instance>
rmdir /var/lib/incus/storage-pools/<pool>/containers/<instance>
```

Delete the `volatile.*` and `image.*` keys from `<instance>.yaml`, then recreate
the instance from it; `init` fills in `image.*` from the image again:

```sh
grep -vE '^  (volatile|image)\.' <instance>.yaml > <instance>.clean.yaml
incus init "$fp" <instance> < <instance>.clean.yaml
incus start <instance>
```

The saved config still holds `user.incus-beacon.update-failed`, so the ref that
failed is not tried again. The image was not at fault, so to let beacon retry
it:

```sh
incus config unset <instance> user.incus-beacon.update-failed
```

`update-status` stays `failed` until the next update records its outcome.

## Verify

`incus list <instance>` shows it `RUNNING` on the old fingerprint
(`incus config get <instance> volatile.base_image`), and
`incus-beacon check --instance <instance>` no longer shows an error (to verify:
the recreate was run by hand after a plain `incus rebuild` race, not after a
beacon update).
