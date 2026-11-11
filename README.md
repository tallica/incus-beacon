# incus-beacon

Watches OCI registries for newer images of your Incus OCI app containers and
reports what can be updated: on the command line, and as `user.incus-beacon.*`
keys on each instance that lazyincus, incus-compose or the `incus` CLI can read.
A lightweight, Incus-focused cousin of watchtower and diun. Automatic updates
are planned; today it only reports.

## Status

Early development; expect breaking changes.

## Requirements

- Incus with OCI app containers (launched from `docker:`, `ghcr:` or another OCI
  remote, or by incus-compose).
- On the command line: an `incus` CLI remote, or a client certificate the server
  trusts.
- As a container: the Incus API on HTTPS (`core.https_address`) and a client
  certificate the server trusts.
- Network access to the registries the instances' images come from.

## Install / run

From source, with Go 1.27 or newer:

```sh
go install github.com/tallica/incus-beacon/cmd/incus-beacon@latest
```

Release archives for Linux and macOS and the container image
`ghcr.io/tallica/incus-beacon` (linux/amd64, linux/arm64) come with each
release.

Try it against the `incus` CLI's default remote. Without `--write` it only
reads:

```sh
incus-beacon check
```

```
PROJECT  INSTANCE  STATUS            IMAGE                         LATEST                    NOTE
default  web       update-available  docker.io/library/nginx:1.27  1.27@sha256:6784fb0834aa
default  cache     current           docker.io/library/redis:7.4   -
```

### Run it on Incus

From a machine whose `incus` CLI has a remote with full access to the server:

```sh
incus-beacon install --remote <remote>
# or, limited to the projects it watches: ... --project default,apps
# or, living in the apps project: ... --remote <remote>:apps
# add --events to check new and rebuilt instances at once, not on the interval
# add --track digest to report only rebuilds of the instance's own tag
# add --notify-url <url> (repeatable) and --notify-errors for notifications
```

In the remote's current project, or the one after the colon, it creates a volume
`incus-beacon-state` owned by uid 65534, a one-time trust token (restricted to
`--project` when given) and an instance `incus-beacon` from
`ghcr.io/tallica/incus-beacon` at this version's `major.minor`. The token
reaches the instance as a file, not as config. The instance gets
`user.incus-beacon.track=semver` when the image tag is a version, so it also
reports newer releases of itself; `--track` overrides that. The instance dials
`core.https_address`, or the address of its bridge when Incus listens on all
addresses; `--api-url` overrides that. `install` waits until the instance has
joined, then removes the spent token file. `incus-beacon uninstall` removes the
instance, its volume and its certificate, finding the instance in whichever
project it is in.

When the instance reports a newer release of itself, install that release of the
CLI and run `incus-beacon upgrade --remote <remote>`. It rebuilds the instance
from the image `install` would pick, or `--image`, keeping its config, volume
and certificate, and waits up to `--wait` (2m) for `/ready`. If that never
answers, it rebuilds the instance back onto the image it ran before. It refuses
an older version tag without `--force`, and an instance with snapshots, which
Incus can't rebuild.

Check it: `incus exec incus-beacon -- wget -q -O - http://127.0.0.1:9153/ready`,
or `curl http://<beacon-ip>:9153/ready` from a host that reaches the instance's
network, such as the Incus host. Add `--project <project>` to `incus exec` when
it lives in another project.

#### By hand

The image runs `serve` as uid 65534, listens on port 9153 and keeps its client
certificate in `/var/lib/incus-beacon`. It joins Incus with a one-time trust
token, which also carries the server's fingerprint for pinning.

1. Create a volume owned by uid 65534, so the certificate survives the instance
   being rebuilt for a new image:

   ```sh
   incus storage volume create default beacon-data initial.uid=65534 initial.gid=65534
   ```

2. Create the instance with a trust token, attach the volume, then start it.
   `--restricted --projects` limits it to the projects it should watch:

   ```sh
   incus remote add ghcr https://ghcr.io --protocol=oci
   incus create ghcr:tallica/incus-beacon beacon \
     -c environment.BEACON_INCUS=https://<incus-host>:8443 \
     -c environment.BEACON_TOKEN="$(incus config trust add incus-beacon --quiet)"
   # or: ... trust add incus-beacon --restricted --projects default,apps --quiet
   incus storage volume attach default beacon-data beacon state /var/lib/incus-beacon
   incus start beacon
   ```

   The first start redeems the token; later starts reuse the saved certificate,
   and the spent token left in the config is no longer usable.

3. Check it as above, with `beacon` as the instance name.

A certificate made by hand works too: give `BEACON_CLIENT_CERT`,
`BEACON_CLIENT_KEY` and `BEACON_SERVER_FINGERPRINT` (from
`incus info | grep certificate_fingerprint`) instead of the token, and push the
files in with `incus file push … --uid 65534 --gid 65534 --mode 0400`.

With incus-compose the same settings go into a service, and a named volume keeps
the certificate. incus-compose's healthd sidecar runs the healthcheck and
records the result in `user.healthcheck.status`:

```yaml
services:
  beacon:
    image: ghcr.io/tallica/incus-beacon:0.4
    environment:
      BEACON_INCUS: https://<incus-host>:8443
      BEACON_TOKEN: ${BEACON_TOKEN}
    volumes:
      - state:/var/lib/incus-beacon
    healthcheck:
      test: ["CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:9153/ready"]
      interval: 1m

volumes:
  state:
```

Put the token in `.env` next to `compose.yaml`, then start it:

```sh
echo "BEACON_TOKEN=$(incus config trust add incus-beacon --quiet)" > .env
incus-compose up -d
```

A token restricted with `--restricted --projects` needs the project to exist, so
run `incus-compose up --no-start` before creating it.

A hand-made certificate can be mounted from a directory on the Incus host
instead, with `BEACON_CLIENT_CERT`, `BEACON_CLIENT_KEY` and
`BEACON_SERVER_FINGERPRINT` in place of the token. incus-compose mounts it with
`shift=true`, which needs idmapped mount support in the host's kernel and file
system (Linux 5.15 or later), and every command against a remote Incus needs
`--allow-remote-mounts`. As root on the Incus host, give the files to uid 65534:

```sh
chown -R 65534:65534 /path/to/certs
chmod 0500 /path/to/certs && chmod 0400 /path/to/certs/*
```

## Usage

```
incus-beacon <command> [flags]
```

| Command     | What it does                                                                                             |
| ----------- | -------------------------------------------------------------------------------------------------------- |
| `check`     | Checks registries and prints what can be updated; `--write` records it on the instances                  |
| `status`    | Prints what is recorded on the instances, without asking any registry                                    |
| `serve`     | Checks on start and every `--interval` (6h, ±10%), records results, serves `/health` `/ready` `/metrics` |
| `install`   | Creates a `serve` instance on Incus that joins with a trust token                                        |
| `upgrade`   | Rebuilds that instance on a newer image, rolling back if it isn't ready                                  |
| `uninstall` | Removes that instance, its volume and its certificate                                                    |
| `version`   | Prints the version                                                                                       |

`incus-beacon <command> -h` lists every flag. Each flag can also be set with an
environment variable named after it: `--server-fingerprint` is
`BEACON_SERVER_FINGERPRINT`; a flag on the command line wins. The ones most
often needed:

| Flag                                       | Purpose                                                                     |
| ------------------------------------------ | --------------------------------------------------------------------------- |
| `--remote`                                 | An `incus` CLI remote; the CLI's default when neither this nor `--incus`    |
| `--remote <remote>:<project>`              | Also the project `install` puts the instance in                             |
| `--incus`, `--client-cert`, `--client-key` | Connect over HTTPS with a trusted client certificate                        |
| `--server-fingerprint`                     | Pin the server's certificate (SHA-256, as `incus info` prints it)           |
| `--token`, `--state-dir`                   | Join with a one-time trust token; keep the certificate it earns in the dir  |
| `--token-file`                             | Read the token from a file instead, as `install` sets up                    |
| `--project`                                | Project to watch, or several comma-separated; all visible ones by default   |
| `--watch`                                  | `all` OCI instances (default), or `opt-in` to watch only `enable=true` ones |
| `--format`                                 | `table` or `json`, for `check` and `status`                                 |
| `--events`                                 | Check new, changed and rebuilt instances at once; `serve` and `install`     |
| `--notify-url`, `--notify-command`         | Where `serve` sends [notifications](#notifications)                         |
| `--registry-auth-file`                     | A docker `config.json`, for `check` and `serve`; docker's usual by default  |
| `--log-level`, `--log-format`              | `debug` to `error`; `console` (default) or `json` (logs go to stderr)       |
| `--log-color`                              | Console log colour: `auto` (default, terminals only), `never` or `always`   |

### Instance keys

Set these on an instance, or on a profile, to change how it is watched. Each can
also be given as `user.label.incus-beacon.<key>`, which is what an incus-compose
`labels:` entry such as `incus-beacon.track: semver` becomes;
`user.incus-beacon.<key>` wins when both are set. A key set on the instance
itself is lost when the instance is recreated, as `incus-compose up --recreate`
does; the label or a profile keeps it. Before 0.5 the keys were `user.beacon.*`
and `user.label.beacon.*`; those are still read, with a warning, until 0.7, and
the status keys below move to the new names the next time an instance is
written.

| Key                            | Values                       | Meaning                                                                  |
| ------------------------------ | ---------------------------- | ------------------------------------------------------------------------ |
| `user.incus-beacon.enable`     | `true`, `false`              | Watch this instance or not, whatever `--watch` says                      |
| `user.incus-beacon.image`      | an image ref                 | The ref to track, when Incus doesn't know it or the image is local       |
| `user.incus-beacon.track`      | `digest` (default), `semver` | `digest` reports a rebuilt tag; `semver` also reports newer version tags |
| `user.incus-beacon.constraint` | e.g. `~1.27`, `<2`           | With `semver`: only versions that satisfy it                             |
| `user.incus-beacon.include`    | a regex                      | With `semver`: only tags that match                                      |
| `user.incus-beacon.exclude`    | a regex                      | With `semver`: no tags that match                                        |

`semver` keeps the shape of the current tag: `1.27.3-alpine` only moves to newer
`x.y.z-alpine` tags, and `1.27` only to newer `x.y` tags. A tag that is not a
version, such as `latest`, needs `digest`.

incus-beacon writes these, and never any other key:

| Key                         | Value                                                                   |
| --------------------------- | ----------------------------------------------------------------------- |
| `user.incus-beacon.status`  | `current`, `update-available`, `error` or `unknown`                     |
| `user.incus-beacon.current` | The image ref being tracked                                             |
| `user.incus-beacon.latest`  | The newer tag, or `<tag>@sha256:<12 hex>` when the same tag was rebuilt |
| `user.incus-beacon.error`   | Why the status is `error` or `unknown`; removed once that clears        |
| `user.incus-beacon.since`   | When the status or latest last changed (RFC 3339)                       |

They change only when a value does, so a quiet check leaves no trace in
`incus monitor`.

### Endpoints

`serve` answers on `--listen` (`:9153`):

| Path       | Answers                                                                                  |
| ---------- | ---------------------------------------------------------------------------------------- |
| `/health`  | 200 while the process runs                                                               |
| `/ready`   | 200 once the last cycle finished without error, else 503; JSON with that cycle's summary |
| `/metrics` | Prometheus metrics, below                                                                |

| Metric                                           | Type      | Labels                   | Meaning                                                          |
| ------------------------------------------------ | --------- | ------------------------ | ---------------------------------------------------------------- |
| `incus_beacon_instances`                         | gauge     | `status`                 | Watched instances by status, as of each one's latest check       |
| `incus_beacon_cycles_total`                      | counter   | `result`                 | Full check cycles, `success` or `failure`                        |
| `incus_beacon_last_success_timestamp_seconds`    | gauge     |                          | When the last successful cycle finished; 0 before the first      |
| `incus_beacon_cycle_duration_seconds`            | gauge     |                          | How long the last cycle took                                     |
| `incus_beacon_status_writes_total`               | counter   | `result`                 | `user.incus-beacon.*` writes, `written` or `failed`              |
| `incus_beacon_registry_requests_total`           | counter   | `host`, `method`, `code` | Registry requests by HTTP status, or `error` when none came back |
| `incus_beacon_registry_request_duration_seconds` | histogram | `host`                   | Time to a registry's response headers                            |
| `incus_beacon_notifications_total`               | counter   | `notifier`, `result`     | Messages per notifier kind, `sent` or `failed`                   |
| `incus_beacon_build_info`                        | gauge     | `version`                | Always 1                                                         |

Targeted checks from `--events` update `incus_beacon_instances` too; an instance
deleted since is counted until the next cycle. Requests held back by a
registry's rate limit are never sent, so they are not counted.

### Notifications

`serve` sends a message when an instance gets an update, or a newer one, and,
with `--notify-errors`, when its check starts failing. Each change is sent once,
and the changes one check finds go in one message. An update applied, or a check
that recovers, sends nothing.

`--notify-url` can be repeated, or hold several URLs separated by spaces, as
`BEACON_NOTIFY_URL` does:

| URL                                      | Sends                                                     |
| ---------------------------------------- | --------------------------------------------------------- |
| `https://example.com/hook`, or `http://` | The JSON payload below, as a POST                         |
| `discord://discord.com/api/webhooks/<…>` | A Discord message: the webhook URL with `discord://`      |
| `slack://hooks.slack.com/services/<…>`   | A Slack message: the incoming webhook URL with `slack://` |

`--notify-command` runs a program for each message, with the payload below on
stdin. It is split on spaces and run without a shell; a non-zero exit is logged.
Use it for any other service, for instance ntfy, with
`--notify-command "/usr/local/bin/notify-ntfy https://ntfy.sh/<topic>"`. This
script needs `jq` and `curl`, which the published image doesn't have:

```sh
#!/bin/sh
# notify-ntfy <topic URL>: send the title and message to an ntfy topic
p=$(cat)
printf '%s' "$p" | jq -r .message |
  curl -fsS -H "Title: $(printf '%s' "$p" | jq -r .title)" --data-binary @- "$1"
```

```json
{
  "time": "2026-11-07T12:00:00Z",
  "server": "node1",
  "title": "incus-beacon on node1: 1 update available",
  "message": "• web (default): nginx:1.27 → 1.31",
  "transitions": [
    {
      "project": "default",
      "instance": "web",
      "image": "docker.io/library/nginx:1.27",
      "status": "update-available",
      "previous_status": "current",
      "latest": "1.31",
      "kind": "tag"
    }
  ]
}
```

`server` is the Incus server's own name (`server_name`, the member in a
cluster). An update's `kind` is `tag` for a newer tag, or `digest` for new
layers under the same tag, as `latest` shows with `<tag>@sha256:…`.
`compose_project` and `compose_service` name an incus-compose instance's stack
and service, so a script can act on the right one. A failed check adds
`"reason"` instead of `kind`. A message that fails to send is logged and counted
in `incus_beacon_notifications_total`, and not sent again. Webhook URLs are
secrets: logs name only the scheme and host.

`--notify-url-file` reads more URLs, one per line, with `#` comments. That is
how `install --notify-url` sets them up: it writes `notify-urls` into the state
volume (mode 0400, uid 65534), so the URLs stay out of the instance config and
survive a rebuild. To change them later, edit a local file and push it, then
restart the instance:

```sh
incus storage volume file push --uid 65534 --gid 65534 --mode 0400 \
  notify-urls <pool> incus-beacon-state/notify-urls
incus restart incus-beacon
```

## Documentation

| Doc                                          | What it covers                   |
| -------------------------------------------- | -------------------------------- |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | Design, components, key flows    |
| [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md)   | Dev setup, make targets, testing |
| [docs/RELEASING.md](docs/RELEASING.md)       | How a release is cut             |
| [docs/runbooks/](docs/runbooks/)             | Symptoms, causes and fixes       |

## Changelog

See [CHANGELOG.md](CHANGELOG.md).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

MIT, see [LICENSE](LICENSE).
