# incus-beacon

Watches OCI registries for newer images of your Incus OCI app containers and
reports what can be updated: on the command line, and as `user.beacon.*` keys on
each instance that lazyincus, incus-compose or the `incus` CLI can read. A
lightweight, Incus-focused cousin of watchtower and diun. Automatic updates are
planned; today it only reports.

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

The image runs `serve` as uid 65534 and listens on port 9153. It needs a client
certificate the Incus server trusts (to verify: the recipe below has not yet
been run end to end as an Incus container).

1. Create a certificate and have Incus trust it. `--restricted --projects`
   limits it to the projects it should watch:

   ```sh
   openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:secp384r1 -nodes \
     -days 3650 -subj /CN=incus-beacon -keyout client.key -out client.crt
   incus config trust add-certificate client.crt --name incus-beacon
   # or: ... --restricted --projects default,apps
   ```

2. Note the server's certificate fingerprint for pinning:

   ```sh
   incus info | grep certificate_fingerprint
   ```

3. Create the instance, give it the certificate files readable by uid 65534
   inside the container (here a host directory as a disk device; the ownership
   needed depends on the instance's idmap), then start it:

   ```sh
   incus remote add ghcr https://ghcr.io --protocol=oci
   incus create ghcr:tallica/incus-beacon beacon \
     -c environment.BEACON_INCUS=https://<incus-host>:8443 \
     -c environment.BEACON_SERVER_FINGERPRINT=<fingerprint> \
     -c environment.BEACON_CLIENT_CERT=/certs/client.crt \
     -c environment.BEACON_CLIENT_KEY=/certs/client.key
   incus config device add beacon certs disk source=/path/to/certs path=/certs readonly=true
   incus start beacon
   ```

4. Check it: `curl http://<beacon-ip>:9153/ready`.

With incus-compose the same settings go into a service:

```yaml
services:
  beacon:
    image: ghcr.io/tallica/incus-beacon:0.1
    environment:
      BEACON_INCUS: https://<incus-host>:8443
      BEACON_SERVER_FINGERPRINT: <fingerprint>
      BEACON_CLIENT_CERT: /certs/client.crt
      BEACON_CLIENT_KEY: /certs/client.key
    volumes:
      - /path/to/certs:/certs:ro
    healthcheck:
      test: ["CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:9153/ready"]
      interval: 1m
```

## Usage

```
incus-beacon <command> [flags]
```

| Command   | What it does                                                                                  |
| --------- | --------------------------------------------------------------------------------------------- |
| `check`   | Checks registries and prints what can be updated; `--write` records it on the instances       |
| `status`  | Prints what is recorded on the instances, without asking any registry                         |
| `serve`   | Checks on start and every `--interval` (6h, ±10%), records results, serves `/health` `/ready` |
| `version` | Prints the version                                                                            |

`incus-beacon <command> -h` lists every flag. Each flag can also be set with an
environment variable named after it: `--server-fingerprint` is
`BEACON_SERVER_FINGERPRINT`; a flag on the command line wins. The ones most
often needed:

| Flag                                       | Purpose                                                                     |
| ------------------------------------------ | --------------------------------------------------------------------------- |
| `--remote`                                 | An `incus` CLI remote; the CLI's default when neither this nor `--incus`    |
| `--incus`, `--client-cert`, `--client-key` | Connect over HTTPS with a trusted client certificate                        |
| `--server-fingerprint`                     | Pin the server's certificate (SHA-256, as `incus info` prints it)           |
| `--project`                                | Project to watch, or several comma-separated; all visible ones by default   |
| `--watch`                                  | `all` OCI instances (default), or `opt-in` to watch only `enable=true` ones |
| `--format`                                 | `table` or `json`, for `check` and `status`                                 |
| `--registry-auth-file`                     | A docker `config.json`, for `check` and `serve`; docker's usual by default  |
| `--log-level`, `--log-format`              | `debug` to `error`; `console` (default) or `json` (logs go to stderr)       |
| `--log-color`                              | Console log colour: `auto` (default, terminals only), `never` or `always`   |

### Instance keys

Set these on an instance, or on a profile, to change how it is watched. Each can
also be given as `user.label.beacon.<key>`, which is what an incus-compose
`labels:` entry such as `beacon.track: semver` becomes; `user.beacon.<key>` wins
when both are set.

| Key                      | Values                       | Meaning                                                                  |
| ------------------------ | ---------------------------- | ------------------------------------------------------------------------ |
| `user.beacon.enable`     | `true`, `false`              | Watch this instance or not, whatever `--watch` says                      |
| `user.beacon.image`      | an image ref                 | The ref to track, when Incus doesn't know it or the image is local       |
| `user.beacon.track`      | `digest` (default), `semver` | `digest` reports a rebuilt tag; `semver` also reports newer version tags |
| `user.beacon.constraint` | e.g. `~1.27`, `<2`           | With `semver`: only versions that satisfy it                             |
| `user.beacon.include`    | a regex                      | With `semver`: only tags that match                                      |
| `user.beacon.exclude`    | a regex                      | With `semver`: no tags that match                                        |

`semver` keeps the shape of the current tag: `1.27.3-alpine` only moves to newer
`x.y.z-alpine` tags, and `1.27` only to newer `x.y` tags. A tag that is not a
version, such as `latest`, needs `digest`.

incus-beacon writes these, and never any other key:

| Key                   | Value                                                                   |
| --------------------- | ----------------------------------------------------------------------- |
| `user.beacon.status`  | `current`, `update-available`, `error` or `unknown`                     |
| `user.beacon.current` | The image ref being tracked                                             |
| `user.beacon.latest`  | The newer tag, or `<tag>@sha256:<12 hex>` when the same tag was rebuilt |
| `user.beacon.error`   | Why the status is `error` or `unknown`; removed once that clears        |
| `user.beacon.since`   | When the status or latest last changed (RFC 3339)                       |

They change only when a value does, so a quiet check leaves no trace in
`incus monitor`.

### Endpoints

`serve` answers on `--listen` (`:9153`):

| Path      | Answers                                                                                  |
| --------- | ---------------------------------------------------------------------------------------- |
| `/health` | 200 while the process runs                                                               |
| `/ready`  | 200 once the last cycle finished without error, else 503; JSON with that cycle's summary |

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
