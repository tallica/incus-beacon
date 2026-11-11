# An instance shows "compose drift"

## Symptom

`incus-beacon status` or `check` lists an incus-compose instance as `error` with
the note `compose drift: runs <tag>, compose file has <tag>`.

## Cause

The instance was rebuilt onto another image by hand, for example with
`incus rebuild`, but its compose file still names the old one. incus-compose
keeps `user.image_alias` at the file's ref, so incus-beacon can't tell which of
the two is wanted. The next `incus-compose up --pull always` deletes the
instance and recreates it on the file's ref, undoing the rebuild.

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

## Verify

`incus-beacon check --instance <instance>` shows `current` or
`update-available`.
