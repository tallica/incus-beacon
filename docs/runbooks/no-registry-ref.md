# An instance shows "no registry ref"

## Symptom

`incus-beacon status` or `check` lists an instance as `unknown` with the note
`no registry ref`, `pinned by digest` or `not pulled from a registry`.

## Cause

incus-beacon couldn't work out which image to track from any of the places it
looks ([image ref resolution](../ARCHITECTURE.md#image-ref-resolution)). An
instance created from a copied or imported image may have none of them.
`pinned by digest` means the instance was launched from `<name>@sha256:…` with
no tag, so there is no newer version to look for. `not pulled from a registry`
means the image was built, imported or published locally.

## Fix

Tell incus-beacon which ref to track. For a local image, only do this when the
same layers were also pushed to that ref:

```sh
incus config set <instance> user.beacon.image=docker.io/library/nginx:1.27
```

For an instance you don't want watched, set `user.beacon.enable=false`.

## Verify

`incus-beacon check --instance <instance>` shows `current` or
`update-available`.
