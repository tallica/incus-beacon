# Backlog

Ideas and follow-up work not yet scheduled. Not a roadmap or a commitment.

Actionable items are checkboxes: `- [ ] **Title** — why it matters`. Delete one
when it lands; what shipped belongs in CHANGELOG.md and the docs. Keep any
decision it records as a plain bullet: plain bullets are context or decisions,
not work. An idea that needs more than a paragraph gets its own file in
[backlogs/](backlogs/) and a one-line item here linking to it.

## Next

- [ ] **lazyincus integration** — show `user.incus-beacon.status` and `latest`
      in the lazyincus instance and image panels, and leave
      `user.incus-beacon.*` out of its config fingerprint as
      `user.healthcheck.*` already is, so beacon writes don't read as drift.
- [ ] **Verify updates of incus-compose instances** — waits on incus-compose's
      image override (lxc/incus-compose#282) reaching a release. Then run an
      update, `up`, `up --pull always` and a config-change recreate, and drop
      the README's "to verify".
- [ ] **Resolve digest-pinned instances after a prune** — Incus 7.6 can pin
      `image.id` by digest from an image's first launch, so an instance beacon
      never updated resolves only through the cached image record and reads "no
      registry ref" once Incus prunes it. Record the tracked ref and its
      fingerprint on each check, as `applied` and `applied-image` already do
      after an update or rollback.

## Later

- [ ] **Stop reading the `user.beacon.*` names in 0.7** — remove the legacy
      prefixes from `internal/policy` and the status-key migration in
      `internal/incus`, and the README sentence that names them.

- [ ] **incus-compose token without `.env`** — needs an incus-compose feature
      that puts a trust token into `/run/secrets/token`; beacon already reads
      that path through `--token-file`.
- [ ] **Update windows by weekday** — `update-window` is daily; a weekly form
      (`Sat 02:00-05:00`) would keep updates to a maintenance day.
- [ ] **Updating compose stacks** — a README section on the two paths that
      change the compose file rather than the instance: Renovate's
      `docker-compose` manager opening PRs for newer tags, or beacon's webhook
      starting a Semaphore UI task (or a CI job) that runs `incus-compose up`.
      Both untested with incus-compose (to verify).
- [ ] **Per-registry credentials and mirrors in instance keys** — for setups
      where one docker `config.json` isn't enough.
- [ ] **Changelog links in notifications** — built from the
      `org.opencontainers.image.source` and version labels, so a notification
      says what changed.

## Not planned

- **beacon editing compose files** — beacon has no access to the repo or host
  that holds them, and Renovate already does this with review in git
  (2026-11-07).
- **Komodo as the update runner** — it drives Docker and `docker compose`
  through its Periphery agent, with no Incus support found (2026-11-07).
- **shoutrrr for notifications** — roughly doubles the binary (about 13 to 25
  MB) and adds many linked modules; built-in senders plus `--notify-command`
  cover the rest. Go plugins can't load into a static `CGO_ENABLED=0` binary, so
  it can't be optional at run time either (2026-11-07).
- **Copying the lazyincus upstream-check code** — the maintainer chose a fresh
  implementation; lazyincus stays a reference (2026-11-04).
- **Unix-socket-only access to Incus** — unproven inside OCI containers and
  grants full admin; a local socket through `--remote` stays available for host
  CLI use (2026-11-04).
- **A YAML config file or a bbolt state database** — flags, environment and
  instance keys cover v0.1, and the instance keys are the state. Revisit if
  per-registry settings outgrow flags (2026-11-04).
- **cobra or urfave/cli** — the standard library `flag` package covers four
  subcommands (2026-11-04).
- **A "last checked" key written every cycle** — see the write-on-change
  invariant in [ARCHITECTURE.md](docs/ARCHITECTURE.md#invariants) (2026-11-04).
