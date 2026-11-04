# An instance shows "no registry ref"

## Symptom

`incus-beacon status` or `check` lists an instance as `unknown` with the note
`no registry ref`, or as `unknown` with `pinned by digest`.

## Cause

incus-beacon couldn't work out which image to track from any of the places it
looks ([image ref resolution](../ARCHITECTURE.md#image-ref-resolution)). An
instance created from a copied or imported image may have none of them.
`pinned by digest` means the instance was launched from `<name>@sha256:…` with
no tag, so there is no newer version to look for.

## Fix

Tell incus-beacon which ref to track:

```sh
incus config set <instance> user.beacon.image=docker.io/library/nginx:1.27
```

For an instance you don't want watched, set `user.beacon.enable=false`.

## Verify

`incus-beacon check --instance <instance>` shows `current` or
`update-available`.
