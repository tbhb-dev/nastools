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
lock_dir = "/example.invalid/locks"

[files]
STATIC_INPUT = "config/static.txt"

[[serve]]
service = "svc:example"
https = 443
target = "http://example.invalid:3000"
```

The default `wait_seconds` is 180. Project names use lowercase letters, digits, the `_` character, and hyphens. They start with a letter or digit.

Set `[ssh].lock_dir` to an existing absolute directory on the network-attached storage device that the SSH user can write. It has no default.

Every required variable and SSH input must have a non-empty value. Static files enter the Compose child environment unchanged. An optional `[expand_files]` table uses the same output-variable-to-path format and expands environment references in memory. Use it only for a backend without native credential-file or file-backed expansion support. Missing expansion inputs and unknown config fields fail the check. Each Serve entry selects either `https` or `tcp`, with a matching HTTP(S) or TCP target, and uses a unique (Service ID, port) pair. A Service ID has the form `svc:` plus a DNS label of lowercase letters, digits, and hyphens, with no leading or trailing hyphen and at most 63 characters. Ports, including the port in a target, range from 1 to 65535.

With a non-empty `GITHUB_ACTIONS` value, every command first reads the config without validating it and emits a mask command for each private value it finds, before any other output. A malformed config still yields its values, and a key matches in any case, as the strict parser accepts it. If the config doesn't read or parse, every error prints only `invalid or missing input`, so no command echoes its arguments. nastools treats as private the value of each required variable and SSH input, the host names in those values, the variables that `[expand_files]` templates reference, each `[files]` line of at least eight characters, and each entry in the comma-separated `NASTOOLS_PRIVATE_STRINGS`. nastools then writes standard output and standard error line by line, including Compose and Serve output, and replaces each private value and each `*.ts.net` host name with `***`. A host name seen for the first time gets its own mask command before its line. A failed mask write stops the command before it starts, or fails it with a nonzero exit later. Line buffering can change how lines from the two streams interleave. nastools refuses the hidden shell-completion commands, which would write past the redaction. Without `GITHUB_ACTIONS`, output passes through unchanged.

Redaction doesn't cover:

- Values nastools never sees, such as the `.env` file that Compose reads on its own, other environment variables Compose interpolates, and secrets that exist only inside containers.
- Transformed forms of a value, such as JSON-escaped, URL-encoded, base64, shell-quoted, partial, or hashed text.
- A Go runtime panic, which writes past the redacting writers.
- The step's `env:` block, which the runner prints before nastools starts. Pass private values through `secrets.*`, not `vars.*`.
- Short values: lines under eight characters within a multi-line value or a `[files]` file, and host names that short. nastools replaces a short value of its own wherever it appears.
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

The deployment lock is `<lock_dir>/<project>.lock`. An SSH session runs `flock -n <lockfile> cat` and holds the lock through status. If the lock is busy, nastools reports a deployment in progress and exits with an error. `nastools deploy --lock-wait 5m` retries for up to five minutes and reports the same error if the lock is still busy then. If the lock session ends for any reason before the run finishes, nastools cancels the run. The default is no wait. Closing the session releases the lock without persistent lock state or a break command. Different projects use different locks. `status` and `logs` don't get one.

Before deployment use, network-attached storage qualification must verify host keys and agent authentication. It must also verify `sudo` without interaction, `flock` release on disconnect, and the pinned Compose/Engine combination.

## Releases

Pushing a `v*` tag starts the release workflow. It builds `nastools` for macOS and Linux on arm64 and amd64 through `mise run build`, retaining the version, commit, and date metadata. Each `nastools_<tag>_<os>_<arch>.tar.gz` archive contains the executable and `LICENSE`. The release includes `checksums.txt` with `sha256sum` hashes for the archives.

After the first release, install a published version with `mise use github:tbhb-dev/nastools@<version>`, replacing `<version>` with the release number without its leading `v`. mise selects the archive for the local platform.
