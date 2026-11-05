# /ready answers 503

## Symptom

`curl http://<beacon>:9153/ready` answers 503, the container health check fails,
and the log has `"message":"cycle done"` with an `error` field. The JSON from
`/ready` carries the same text in `last_error`.

## Cause

The last cycle could not list instances, so it checked nothing. Usually one of:

- `tls: failed to verify certificate: x509: …`: no `--server-fingerprint`, and
  the server's certificate is self-signed, as Incus's is by default.
- `server certificate fingerprint … does not match --server-fingerprint`: the
  pin is wrong, or the server certificate changed.
- `the server doesn't trust this client certificate`: the certificate was never
  added, or was removed.
- `project …: not found, or this certificate may not use it`: `--project` names
  a project that doesn't exist, or (to verify) one the certificate is not
  restricted to.
- `connection refused` or a timeout: Incus isn't listening on that address
  (`core.https_address`), or a firewall is in the way.

A single instance whose registry fails doesn't cause this; it shows as `error`
on that instance only.

## Fix

1. Read the fingerprint on the server and set it:
   `incus info | grep certificate_fingerprint`, then
   `BEACON_SERVER_FINGERPRINT=<that value>`.
2. Check that the certificate is trusted and for which projects:
   `incus config trust list`, `incus config trust show <fingerprint>`.
3. Check that Incus listens on HTTPS: `incus config get core.https_address`.

## Verify

The next cycle (or a restart, which runs one at once) logs `cycle done` without
`error`, and `/ready` answers 200.
