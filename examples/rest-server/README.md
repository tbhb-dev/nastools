# Append-only restic endpoint

This example deploys a [restic rest-server](https://github.com/restic/rest-server) that a backup client can append to and can't rewrite or delete from. A compromised client can add data, but it can't destroy the backups it already wrote.

The stack is `compose.yaml` and `nastools.toml`, with placeholders only. A consuming repository keeps real values in its environment and in a local file outside version control.

## Image

Pinned by digest, the `rest-server` service runs `restic/rest-server`. That digest, `sha256:d2aff06f47eb38637dff580c3e6bce4af98f386c396a25d32eb6727ec96214a5`, is the multi-architecture index of the `0.14.0` tag, which `latest` also pointed to on 2026-10-22. Test the server version with the client version you run, and update the digest deliberately instead of following a tag.

Running `--append-only --path /data --htpasswd-file /run/secrets/htpasswd`, the server requires HTTP basic authentication for every request. Because the image entrypoint is a script that adds its own options, the Compose file swaps it for the `rest-server` binary.

The service listens on `127.0.0.1:8000` only. The health check sends an HTTP request and accepts any response, including a 401. It passes before any repository exists. A check on repository content would block `nastools deploy`, because the client can create the repository only after the deploy.

## Inputs

| Input | Kind | Meaning |
| --- | --- | --- |
| `REST_SERVER_DATA_PATH` | Required variable | Absolute host path of the repository data, mounted at `/data`. |
| `REST_SERVER_UID` | Required variable | Host user ID with write access to the path. The container runs as this user. |
| `REST_SERVER_GID` | Required variable | Host group ID of the path. The container runs in this group. |
| `HTPASSWD` | File input | The client `htpasswd` file, read from `secrets/htpasswd` next to `nastools.toml`. |
| `SSH_ADDRESS`, `SSH_USER`, `SSH_KNOWN_HOSTS`, `SSH_LOCK_DIR` | SSH inputs | The deployment target, as in the main README. |

Create the data path on the device with write access for that user ID and group ID. This stack refers to no real share, user, or path.

Compose delivers the `htpasswd` file to the container as a secret with an `environment:` source, which nastools fills from the `HTPASSWD` file input. Keep every credential out of a service's `environment:`, `command:`, and labels. Create the file with `htpasswd -B -c <file> <client user>` from the Apache utilities, and keep it outside version control. The repository password isn't an input of this stack and stays off the device. Only the backup client stores it.

Check the stack with example inputs:

```sh
export REST_SERVER_DATA_PATH=/srv/example
export REST_SERVER_UID=1000
export REST_SERVER_GID=1000
export SSH_ADDRESS=nas.example.invalid:22
export SSH_USER=deploy
export SSH_KNOWN_HOSTS="nas.example.invalid ssh-ed25519 AAAA"
export SSH_LOCK_DIR=/var/lib/nastools/locks
nastools check --config examples/rest-server/nastools.toml
```

## Tailscale service

The `[[serve]]` entry publishes `svc:rest-server` over HTTPS on port 443 and forwards to `http://127.0.0.1:8000`. Define the Service in the Tailscale administration console, give the deploy user Serve authority for it, and approve the device to advertise the Service. Approval comes from auto-approval for the device's node tag or from the console.

The access policy of your Tailscale network should grant only the source host's tag access to the Service. With a source tag of `tag:backup-source`, the grant looks like this:

```json
{
  "grants": [
    {
      "src": ["tag:backup-source"],
      "dst": ["svc:rest-server"],
      "ip": ["tcp:443"]
    }
  ]
}
```

With no other grant for the Service, a local network client and a node without that tag can't connect to the endpoint.

## Bootstrap a repository

Append-only mode permits creating a repository, so initialize a fresh one from the client. Pass the credentials through the environment to keep them out of the URL and the shell history:

```sh
export RESTIC_REPOSITORY=rest:https://rest-server.example.invalid/repo
export RESTIC_REST_USERNAME=client
export RESTIC_REST_PASSWORD_FILE=/path/to/client-password
export RESTIC_PASSWORD_FILE=/path/to/repository-password
restic init
restic backup /path/to/data
restic snapshots
```

Replace the repository URL with the address of the Service. The client password matches the `htpasswd` entry. The repository password is a different secret.

## Restore

Restore from the client into a scratch directory, then compare checksums against the source:

```sh
restic restore latest --target /path/to/scratch
```

## Verify append-only mode

Run these from the client with the client credential. Each must fail:

- `restic forget <snapshot ID>` on a disposable snapshot fails with a permission error, because the server refuses to delete a snapshot file.
- Overwriting an existing object in the repository fails, because the server refuses to replace a file that exists.
- A request with a wrong password gets a 401 response.

`restic backup` and `restic snapshots` still work with the same credential.

## Retention

This stack doesn't run retention. The device never runs `restic forget` or `restic prune`, and the repository only grows. Check the quota and free space of the share before relying on the endpoint.

Recovery relies on the Synology DiskStation Manager snapshots of the share, which don't depend on the endpoint credential. Schedule daily snapshots and keep them for at least 30 days.

Any retention runs from a trusted host that holds the repository password, never on the device. The restic documentation on [append-only mode](https://restic.readthedocs.io/en/stable/060_forget.html#security-considerations-in-append-only-mode) recommends a keep-within policy, because a compromised client can add snapshots that push legitimate ones out of a count-based policy.
