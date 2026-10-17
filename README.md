# nastools

nastools deploys a Compose stack to Synology over SSH, with strict host identity checks and file-based configuration and secrets. It preserves persistent bind-mounted data and checks service readiness. Declared Tailscale Services expose healthy services. Rollback redeploys an earlier compatible commit.
