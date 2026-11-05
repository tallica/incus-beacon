# Instances show "rate limited until …"

## Symptom

`incus-beacon check` or `status` shows instances as `error` with a note like
`rate limited until 2026-11-04T18:00:00Z`, usually all from one registry.

## Cause

The registry answered 429 Too Many Requests, or reported no pulls left
(`ratelimit-remaining: 0`). incus-beacon then pauses manifest downloads from
that registry until the time shown, or for an hour when the registry gives none.
Docker Hub allows anonymous users 100 manifest GETs per 6 hours per IP address;
HEAD requests, which incus-beacon tries first, don't count. Many instances whose
tags moved at once, or other clients behind the same address, use the allowance
up.

## Fix

1. Wait: the pause lifts at the time shown, and the last known `latest` stays on
   each instance meanwhile.
2. Log in for a higher allowance. Create a docker `config.json` with
   `docker login`, and point incus-beacon at it with `--registry-auth-file` (in
   a container, mount it and set `BEACON_REGISTRY_AUTH_FILE`).
3. If many replicas are involved, nothing more is needed: replicas of one image
   already share one lookup.

## Verify

`incus-beacon check --instance <name> --log-level debug` shows the instance as
`current` or `update-available` again.
