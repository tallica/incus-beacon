# An instance shows "compose drift"

## Symptom

`incus-beacon status` or `check` lists an incus-compose instance as `error` with
the note `compose drift: runs <tag>, compose file has <tag>`, or, after
incus-beacon updated it, `drift: runs <tag>, incus-beacon updated it to <tag>`.

## Cause

The instance was rebuilt onto another image by hand, for example with
`incus rebuild`, but its compose file still names the old one. incus-compose
keeps `user.image_alias` at the file's ref, so incus-beacon can't tell which of
the two is wanted. The next `incus-compose up --pull always` deletes the
instance and recreates it on the file's ref, undoing the rebuild.

With `incus-beacon updated it to`, incus-beacon had updated the instance to the
ref in `user.incus-beacon.applied`, and it was rebuilt onto another one by hand
since.

## Fix

To go back to the file's ref, apply the file; incus-compose sees the image
differs and recreates the instance:

```sh
incus-compose up -d --pull always
```

To keep the rebuild, set the service's `image:` to the ref the instance runs,
then recreate it. Plain `up` finds the image already matching and leaves the
stale `user.image_alias` in place:

```sh
incus-compose up -d --recreate
```

After incus-beacon's update, either rebuild onto the applied ref again, from an
OCI remote such as `docker:`:

```sh
incus stop <instance>
incus rebuild <remote>:<applied ref> <instance>
incus start <instance>
```

or drop the update record, to go back to tracking the compose file's ref:

```sh
incus config unset <instance> user.incus-beacon.applied
incus config unset <instance> user.incus-beacon.requested
```

## Verify

`incus-beacon check --instance <instance>` shows `current` or
`update-available`.
