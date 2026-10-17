# nastools

nastools deploys a Compose stack to Synology over SSH, with strict host identity checks and file-based configuration and secrets. It preserves persistent bind-mounted data and checks service readiness. Declared Tailscale Services expose healthy services. Rollback redeploys an earlier compatible commit.

## Development

The current binary exposes only `version`. Deployment commands belong to later phases. [mise](https://mise.jdx.dev) pins the toolchain and runs the tasks. Run `mise run bootstrap` once, then `mise run check` before pushing. [AGENTS.md](AGENTS.md) is the contributor guide.

Build `bin/nastools` with `mise run build`, then run `bin/nastools version`. For a source run with the same metadata flags, use `mise run run version`. A plain `go run ./cmd/nastools version` uses the runtime build-info fallback for the version and leaves the commit and date empty.

Go checks run through `mise run lint-go-all`, with a separate formatting check at `mise run lint-go-fmt`. Coverage thresholds apply through `mise run cover-go`. To check for data races, use `mise run test-go-race`. Refresh dependencies with `mise run vendor`, then check for dependency drift with `mise run vendor-check`. The drift gate also detects files outside the index. Go tool directives pin analysis tools, whose sources join app dependencies in `vendor/`.

## Releases

Pushing a `v*` tag starts the release workflow. It builds `nastools` for macOS and Linux on arm64 and amd64 through `mise run build`, retaining the version, commit, and date metadata. Each `nastools_<tag>_<os>_<arch>.tar.gz` archive contains the executable and `LICENSE`. The release includes `checksums.txt` with `sha256sum` hashes for the archives.

After the first release, install a published version with `mise use github:tbhb-dev/nastools@<version>`, replacing `<version>` with the release number without its leading `v`. mise selects the archive for the local platform. The binary still exposes only `version`.
