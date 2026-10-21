# nastools

nastools deploys a Compose stack to Synology over SSH, with strict host identity checks and file-based configuration and secrets. It preserves persistent bind-mounted data and checks service readiness. Declared Tailscale Services expose healthy services. Rollback redeploys an earlier compatible commit.

## Development

The current binary exposes `version` and `check`. Deployment commands belong to later phases. [mise](https://mise.jdx.dev) pins the toolchain and runs the tasks. Run `mise run bootstrap` once, then `mise run check` before pushing. [AGENTS.md](AGENTS.md) is the contributor guide.

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

[files]
STATIC_INPUT = "config/static.txt"

[[serve]]
service = "svc:example"
https = 443
target = "http://example.invalid:3000"
```

The default `wait_seconds` is 180.

Every required variable and SSH input must have a non-empty value. Static files enter the Compose child environment unchanged. An optional `[expand_files]` table uses the same output-variable-to-path format and expands environment references in memory. Use it only for a backend without native credential-file or file-backed expansion support. Missing expansion inputs and unknown config fields fail the check. Each Serve entry selects either `https` or `tcp`, with a matching HTTP(S) or TCP target, and uses a unique (Service ID, port) pair.

With a non-empty `GITHUB_ACTIONS` value, the check emits mask commands for required values, SSH inputs, and comma-separated `NASTOOLS_PRIVATE_STRINGS` before validation output. Git state supplies the commit label, with `-dirty` for modified or new files. Deployment and lock waiting belong to later phases.

## Releases

Pushing a `v*` tag starts the release workflow. It builds `nastools` for macOS and Linux on arm64 and amd64 through `mise run build`, retaining the version, commit, and date metadata. Each `nastools_<tag>_<os>_<arch>.tar.gz` archive contains the executable and `LICENSE`. The release includes `checksums.txt` with `sha256sum` hashes for the archives.

After the first release, install a published version with `mise use github:tbhb-dev/nastools@<version>`, replacing `<version>` with the release number without its leading `v`. mise selects the archive for the local platform.
