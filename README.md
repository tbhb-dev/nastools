# nastools

nastools deploys a Compose stack to Synology over SSH, with strict host identity checks and file-based configuration and secrets. It preserves persistent bind-mounted data and checks service readiness. It publishes declared Tailscale Serve endpoints and labels each deployment with its commit. Rollback redeploys an earlier compatible commit.

## Development

The current binary exposes `version`, `check`, `deploy`, `status`, and `logs`. [mise](https://mise.jdx.dev) pins the toolchain and runs the tasks. Run `mise run bootstrap` once, then `mise run check` before pushing. [AGENTS.md](AGENTS.md) is the contributor guide.

Build `bin/nastools` with `mise run build`, then run `bin/nastools version`. For a source run with the same metadata flags, use `mise run run version`. A plain `go run ./cmd/nastools version` uses the runtime build-info fallback for the version and leaves the commit and date empty.

Go checks run through `mise run lint-go-all`, with a separate formatting check at `mise run lint-go-fmt`. Coverage thresholds apply through `mise run cover-go`. To check for data races, use `mise run test-go-race`. Refresh dependencies with `mise run vendor`, then check for dependency drift with `mise run vendor-check`. The drift gate also detects files outside the index. Go tool directives pin analysis tools, whose sources join app dependencies in `vendor/`.

## Check inputs

Run `nastools check --config path/to/nastools.toml` from the deployment checkout. With Git and Docker Compose installed, this checks the inputs and runs `docker compose config -q` without an SSH connection. It discards Compose output because parser errors can contain secret values. The default config path is `nastools.toml`, and paths within it resolve from its directory.

```toml
project = "example"
compose = ["compose.yaml"]
wait_seconds = 180
required_env = ["REQUIRED_INPUT"]

[ssh]
address_env = "SSH_ADDRESS"
user_env = "SSH_USER"
known_hosts_env = "SSH_KNOWN_HOSTS"
lock_dir_env = "SSH_LOCK_DIR"

[files]
STATIC_INPUT = "config/static.txt"

[[serve]]
service = "svc:example"
https = 443
target = "http://example.invalid:3000"
```

The default `wait_seconds` is 180. Project names use lowercase letters, digits, the `_` character, and hyphens. They start with a letter or digit.

The lock directory is an existing absolute directory on the network-attached storage device that the SSH user can write. The example sets `[ssh].lock_dir_env` to the name of an environment variable whose value is that path. A config can instead set `[ssh].lock_dir` to the path itself. A config sets exactly one of the two, and neither has a default. The variable that `lock_dir_env` names is an SSH input, and its value must be an absolute path, as a `lock_dir` path must be.

Every required variable and SSH input must have a non-empty value. Static files enter the Compose child environment unchanged. An optional `[expand_files]` table uses the same output-variable-to-path format and expands environment references in memory. Use it only for a backend without native credential-file or file-backed expansion support. Missing expansion inputs and unknown config fields fail the check. The config and each `[files]` and `[expand_files]` input must be a regular file of at most 1 MiB. nastools checks the type before it opens one, so a named pipe, a device, or a larger file fails as unreadable instead of blocking the run. Compose files go to Compose unchanged, without this check. Each Serve entry selects either `https` or `tcp`, with a matching HTTP(S) or TCP target, and uses a unique (Service ID, port) pair. A Service ID has the form `svc:` plus a DNS label of lowercase letters, digits, and hyphens, with no leading or trailing hyphen and at most 63 characters. Ports, including the port in a target, range from 1 to 65535.

With a non-empty `GITHUB_ACTIONS` value, every command first reads the config without validating it and emits a mask command for each private value it finds, before any other output. A malformed config still yields its values, and a key matches in any case, as the strict parser accepts it. If the config doesn't read or parse, every error prints only `invalid or missing input`, so no command echoes its arguments. nastools treats as private the value of each required variable and SSH input, the host names in the SSH address and known-hosts values, the variables that `[expand_files]` templates reference, each `[files]` line of at least eight bytes, and each entry in the comma-separated `NASTOOLS_PRIVATE_STRINGS`. nastools then writes standard output and standard error line by line, including Compose and Serve output, and replaces each private value and each `*.ts.net` host name, in any case, with `***`. A host name seen for the first time gets its own mask command before its line. A failed mask write stops the command before it starts, or fails it with a nonzero exit later. Line buffering can change how lines from the two streams interleave. nastools refuses the hidden shell-completion commands, which would write past the redaction. Without `GITHUB_ACTIONS`, output passes through unchanged.

Redaction doesn't cover:

- Values nastools never sees, such as the `.env` file that Compose reads on its own, other environment variables Compose interpolates, and secrets that exist only inside containers.
- Transformed forms of a value, such as JSON-escaped, URL-encoded, base64, shell-quoted, partial, or hashed text.
- A Go runtime panic, which writes past the redacting writers.
- Go runtime diagnostics that `GODEBUG` turns on, which also write past the redacting writers. The resolver setting `netdns=2`, or its `go+2` and `cgo+2` forms, prints the SSH host name to standard error. Leave `GODEBUG` unset in the step.
- The step's `env:` block, which the runner prints before nastools starts. Pass private values through `secrets.*`, not `vars.*`.
- Standard output that doesn't reach the runner. Under GitHub Actions, standard output carries each private value inside its `::add-mask::` line. Redirecting it to a file, or through `tee` to a file that a later step uploads as an artifact, exposes every value. Leave the standard output of nastools connected to the runner.
- Short values: lines under eight bytes within a multi-line value or a `[files]` file, and host names that short. nastools replaces a short value of its own wherever it appears.
- Application output in `logs`, beyond the known values.

Git state supplies the commit label, with `-dirty` for modified or new files.

## Deploy and inspect

Run `nastools deploy --config path/to/nastools.toml` to check inputs, get the project lock, pull images with `--ignore-buildable`, bring up services with `--wait --wait-timeout <wait_seconds> --remove-orphans`, and publish the `[[serve]]` endpoints. It then prints `compose ps` and the Serve status. Each step must succeed before the next starts. Compose output streams to standard output and standard error. `config -q` output stays discarded.

Deployments aren't transactional. A Serve failure exits nonzero after Compose has already brought the services up, and the error reports the failing entry as `serve[N]`. Fix the cause and run `deploy` again.

### Provenance

`check`, `deploy`, `status`, and `logs` pass the checkout's commit to Compose as `NASTOOLS_COMMIT`, with `-dirty` appended for modified or new files. Reference it in a service label to record the deployed commit:

```yaml
services:
  example:
    labels:
      example.invalid/commit: ${NASTOOLS_COMMIT}
```

Compose recreates a service whose label changes. A labelled service restarts on every new commit. Label only the services that can tolerate that.

### Serve

After `compose up --wait` succeeds, `deploy` applies each `[[serve]]` entry with one command over an SSH session on the existing transport. An `https` entry runs `tailscale serve --service=<service> --https=<port> <target>`, and a `tcp` entry runs `tailscale serve --service=<service> --tcp=<port> <target>`. Services run in the background without `--bg`. The commands run without `sudo`. Only Docker access needs it, and the SSH user must hold Serve authority for each declared Service ID. Repeating a command with the same flags keeps the configuration unchanged. nastools names only declared Service IDs and doesn't reset Serve.

nastools doesn't remove endpoints. After deleting an entry, remove its listener by hand, scoped to that Service ID and port: `tailscale serve --service=<service> --https=<port> off`, or `--tcp=<port>` for a TCP entry.

Inspection commands also accept `--config`:

- `nastools status` prints `compose ps`, then the `tailscale serve status --json` entries for the configured Service IDs. It leaves out other Service IDs.
- `nastools logs [service] --tail N --follow` streams native Compose logs.
- The default tail is 100 lines per service. Omit `--follow` to return after printing them.

SSH setup:

- Authentication uses the agent at `SSH_AUTH_SOCK`.
- The configured address input must include a port, such as `nas.example.invalid:22`.
- The configured known-hosts input contains `known_hosts` text, including more than one line. Host keys must match those entries.
- nastools doesn't read SSH config or invoke an SSH binary or SSH File Transfer Protocol (SFTP) client.

Each run creates a private temporary directory with mode 700 and a local Unix socket. Each socket connection opens an SSH session running `sudo -n /usr/local/bin/docker system dial-stdio`. Local Compose uses that socket through `DOCKER_HOST` and receives the checked input environment. nastools replaces inherited `DOCKER_HOST` and removes `DOCKER_CONTEXT`, `DOCKER_TLS`, `DOCKER_TLS_VERIFY`, and `DOCKER_CERT_PATH` for this connection. Normal completion, errors, SIGINT, and SIGTERM close the transport and remove its directory.

The deployment lock is `<lock directory>/<project>.lock`. An SSH session runs `flock -n <lockfile> cat` and holds the lock through status. If the lock is busy, nastools reports a deployment in progress and exits with an error. `nastools deploy --lock-wait 5m` retries for up to five minutes and reports the same error if the lock is still busy then. If the lock session ends for any reason before the run finishes, nastools cancels the run. The default is no wait. Closing the session releases the lock without persistent lock state or a break command. Different projects use different locks. `status` and `logs` don't get one.

Before deployment use, network-attached storage qualification must verify host keys and agent authentication. It must also verify `sudo` without interaction, `flock` release on disconnect, and the pinned Compose/Engine combination.

## Remote engine

nastools runs Compose on the machine that runs nastools and points it at the device's engine through `DOCKER_HOST`. Each source in a Compose model comes either from that local machine or from the device, and Compose uses it at one fixed step, which the table names. These rows follow the Compose v5.6.1 source. Naming that release doesn't make it the oldest one nastools supports. Testing covers these routes from Compose 2.23.1 onward, and nastools refuses an older release before it connects.

| Source | Content comes from | Compose uses it |
| --- | --- | --- |
| `env_file`, the project `.env` file, and variable interpolation | The local machine | While parsing the model |
| `configs` with `content:` or `environment:` sources, and `secrets` with `environment:` sources | The local machine | Copied into the container after Compose creates it and before it starts |
| Local build contexts | The local machine | Sent when building to the selected builder, which is the device's engine only with the default builder for `DOCKER_HOST` |
| Bind-mount sources and, outside swarm mode, `configs` and `secrets` with `file:` sources, which become bind mounts | The device, at the path resolved on the local machine | When the engine creates the container |

Compose resolves a relative path against the project directory, which nastools sets to the config's directory. The engine then mounts that same absolute path from the device, so the file has to exist there at that path. A `file:` config or secret that's missing on the device fails container creation. For a `file:` secret that's missing on the local machine, Compose only warns, and it doesn't check a `file:` config there. A bind mount whose source is missing on the device doesn't fail: the engine creates an empty directory there, unless the long syntax sets `bind.create_host_path: false`. Because Compose reads `env_file` while parsing, it can never read an `env_file` that a service generates on the device. A missing `env_file` fails the parse unless its long form sets `required: false`, which only skips it. That form needs Compose 2.24.0 or later.

To generate a secret on the device, run an init one-shot that writes into a named volume only when the file is missing. Services mount the volume and read the secret as a file, and `depends_on` with `condition: service_completed_successfully` starts them after the init:

```yaml
services:
  init-secrets:
    image: busybox:1.37
    command:
      - sh
      - -c
      - test -e /secrets/token || { head -c 24 /dev/random | base64 > /secrets/token.tmp && sync /secrets/token.tmp && mv /secrets/token.tmp /secrets/token; }
    volumes:
      - secrets:/secrets
  example:
    image: example.invalid/app
    depends_on:
      init-secrets:
        condition: service_completed_successfully
    volumes:
      - secrets:/run/app-secrets:ro
volumes:
  secrets:
```

The guard matters because `up` starts the exited one-shot again on every deploy, and if the one-shot has the commit label, Compose recreates it on every commit. An init without the guard would replace the secret each time. The guard tests only that the file exists, so it keeps an existing file even when that file is empty. Writing to a temporary file and flushing it with `sync` before `mv` renames it keeps an interrupted init or a power loss from leaving a partial or empty secret that the guard would then keep. The secret lasts as long as the volume, and `compose down -v` removes both.

File content can reach a service without a file on the device through these routes:

- `configs.content`.
- `configs.environment` or `secrets.environment`, fed from a `[files]` input.
- An image built from a local build context.
- An init one-shot that writes a volume from an environment variable.

An init one-shot fed from an environment variable leaves the value in the container's configuration on the device, where `docker inspect` shows it. When that matters, use `secrets.environment` instead.

Each `[files]` value and each `[expand_files]` output reaches Compose as one environment string, so the system running Compose limits its size. On Linux, the kernel limits one environment string, counting the name, the `=`, the value, and the ending null byte, to 32 pages. With 4 KiB pages that's 128 KiB, well below the 1 MiB file cap. Linux also caps arguments and environment together, at a size derived from the stack limit, so two large values can fail together where each would fit alone. macOS limits only arguments and environment together, to the system's `ARG_MAX`. The init one-shot route also passes the value into the container's environment, so the device's Linux kernel applies the same limits when the container process starts, and a larger macOS `ARG_MAX` doesn't raise that ceiling.

## Rehearse a deploy

The SSH engine fixture in `testdata/fixture/` is the supported way to rehearse a deploy against a local Docker engine. It's a container image that runs `sshd` with public-key authentication only, for the user `nastools`. That user may run exactly `sudo -n /usr/local/bin/docker system dial-stdio` without a password, and the Docker command-line tool inside reaches the local engine through its mounted socket. Besides `flock` and the lock directory `/var/lib/nastools/locks`, the image provides a `tailscale` stub. It records each `tailscale serve` command in `/var/lib/nastools/serve.log` and answers `tailscale serve status --json` from that log. A rehearsal records Serve entries without applying them.

Keys don't come with the image. Generate a host key and a client key in a temporary directory, copy the host key and the client's public key into the container before it starts, and publish `sshd` on `127.0.0.1`:

```sh
keys="$(mktemp -d)"
ssh-keygen -q -t ed25519 -N '' -f "$keys/fixture_host_key"
ssh-keygen -q -t ed25519 -N '' -f "$keys/fixture_client_key"
docker build --tag nastools-fixture testdata/fixture
docker create --name nastools-fixture --publish 127.0.0.1:2222:22 \
  --volume /var/run/docker.sock:/var/run/docker.sock nastools-fixture
docker cp "$keys/fixture_host_key" nastools-fixture:/etc/ssh/ssh_host_ed25519_key
docker cp "$keys/fixture_client_key.pub" nastools-fixture:/home/nastools/.ssh/authorized_keys
docker start nastools-fixture
```

nastools reads the client key only from the agent at `SSH_AUTH_SOCK`, and it checks the host key against the known-hosts input. With the SSH section of the example config, the inputs are:

```sh
eval "$(ssh-agent -s)"
ssh-add "$keys/fixture_client_key"
export SSH_ADDRESS=127.0.0.1:2222
export SSH_USER=nastools
export SSH_KNOWN_HOSTS="[127.0.0.1]:2222 $(cat "$keys/fixture_host_key.pub")"
export SSH_LOCK_DIR=/var/lib/nastools/locks
nastools deploy --config path/to/nastools.toml
```

A known-hosts entry for a port other than 22 puts the host in brackets. Afterwards, run `docker compose --project-name <project> down` against the local engine, remove the container with `docker rm --force nastools-fixture`, stop the agent with `ssh-agent -k`, and remove the keys with `rm -r "$keys"`.

Mounting the socket gives the fixture full control of the local engine. Publish its port on `127.0.0.1` only, and use keys made for the rehearsal. Because the fixture's engine is the local one, bind-mount sources and `file:` configs and secrets resolve on this machine. A rehearsal doesn't show that they exist on the device, and it doesn't replace the device qualification that the Serve section describes.

`mise run test-e2e` builds the fixture and runs `check`, `deploy`, `status`, and `logs` through it against the sample project in `testdata/fixture/project/`. It also covers lock contention, the release of the lock a killed deploy held, and a host key mismatch. It needs a Docker engine, and CI runs it on Linux.

## Releases

Pushing a `v*` tag starts the release workflow. It builds `nastools` for macOS and Linux on arm64 and amd64 through `mise run build`, retaining the version, commit, and date metadata. Each `nastools_<tag>_<os>_<arch>.tar.gz` archive contains the executable and `LICENSE`. The release includes `checksums.txt` with `sha256sum` hashes for the archives.

After the first release, install a published version with `mise use github:tbhb-dev/nastools@<version>`, replacing `<version>` with the release number without its leading `v`. mise selects the archive for the local platform.
