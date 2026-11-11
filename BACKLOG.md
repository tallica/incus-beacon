# Backlog

Ideas and follow-up work not yet scheduled. Not a roadmap or a commitment.

Actionable items are checkboxes: `- [ ] **Title** — why it matters`. Delete one
when it lands; what shipped belongs in CHANGELOG.md and the docs. Keep any
decision it records as a plain bullet: plain bullets are context or decisions,
not work. An idea that needs more than a paragraph gets its own file in
[backlogs/](backlogs/) and a one-line item here linking to it.

## Next

- [ ] **lazyincus integration** — show `user.beacon.status` and `latest` in the
      lazyincus instance and image panels, and leave `user.beacon.*` out of its
      config fingerprint as `user.healthcheck.*` already is, so beacon writes
      don't read as drift.

## Later

- [ ] **incus-compose token without `.env`** — needs an incus-compose feature
      that puts a trust token into `/run/secrets/token`; beacon already reads
      that path through `--token-file`.
- [ ] **[Automatic updates](backlogs/automatic-updates.md)** — back up, rebuild
      on the newer image, health-gate, and roll back; opt-in per instance, never
      for incus-compose instances, and only on Incus 7.6 or later.
- [ ] **Updating compose stacks** — a README section on the two paths that
      change the compose file rather than the instance: Renovate's
      `docker-compose` manager opening PRs for newer tags, or beacon's webhook
      starting a Semaphore UI task (or a CI job) that runs `incus-compose up`.
      Both untested with incus-compose (to verify).
- [ ] **Per-registry credentials and mirrors in instance keys** — for setups
      where one docker `config.json` isn't enough.
- [ ] **Cooldown** — report a new tag only once it is a set age, so a broken
      release pulled within hours never triggers an update.
- [ ] **Changelog links in notifications** — built from the
      `org.opencontainers.image.source` and version labels, so a notification
      says what changed.

## Not planned

- **Rebuilding incus-compose instances** — the compose file is the desired
  state; why is in [automatic-updates.md](backlogs/automatic-updates.md#scope)
  (2026-11-07).
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
