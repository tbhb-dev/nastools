# Agent instructions

Guidance for AI coding agents working in this repository. This file is the canonical source for every harness. `CLAUDE.md` points here, and the agy command-line tool reads only this file.

## Scope

nastools is a Go project with one command-line binary, `nastools`. The module path is `github.com/tbhb-dev/nastools`. The binary has its own directory under `cmd/`, `internal/` holds private packages, and `tools/` holds the bash helpers the shared devtools payload lacks. The repository has no Python or Node setup.

## Public repository

This repository is public. Never commit a hostname, an address outside the documentation ranges, an account, project or organization identifier, a token, a password-manager reference, or any other credential. Placeholders use `example.invalid` and the documentation address ranges in Request for Comments (RFC) 5737. Private values live only in `mise.local.toml`, which a gitignore entry keeps out of history, and tasks read them from the environment through `NASTOOLS_PRIVATE_STRINGS`. Copy `mise.local.toml.example` to start one. `mise run lint-leaks` scans the working tree for those values and for private-network and credential patterns, and gitleaks scans the history with `mise run devtools:gitleaks`.

## Tools

mise pins the tools and defines the tasks. The shared half arrives from `tbhb-dev/devtools` through vendir into `.config/mise/conf.d/` and `.devtools/`. Never edit those two directories by hand. Run `mise run bootstrap` once. It syncs the payload, then installs the tools and the git hooks.

- `mise run lint` runs every lint gate in order.
- `mise run test` runs the test suites.
- `mise run check` runs both.
- `mise run lint-draft <file>` runs every prose gate over one draft.
- `mise run devtools:update-pins <version>` moves every devtools pin together. Redeploy the agent primitives with `mise run apm-sync` instead of a bare `apm install`.

A task in `mise.toml` that replaces or narrows a payload task has a comment with the number of the devtools issue behind it. The `guard-markdown` binary must be on PATH for `lint-markdown-wrap` and the Claude write hook. `mise run bootstrap` installs it through `mise run install-guard-markdown`, which takes the version from the devtools pin in `apm.yml`. Keep the Go bin directory on PATH.

## Commit messages

Run the `commit` skill. Draft the message in the repo-root file `COMMIT_AGENTMSG`, which a gitignore entry keeps out of history, then run `mise run lint-commit-msg`. The prek commit-msg hook is the real gate. Stage paths by name, and never run `git add -A` or `git add .`.

A message needs a Conventional Commits subject, a blank line before the body, and a `Signed-off-by:` trailer. An `Assisted-by:` trailer in the form `agent:model tool` is optional, and it goes before the sign-off. Never use `--no-verify` on a real commit.

## Pull requests

Run the `pr` skill. Draft the description in the repo-root file `PR_AGENTDESC.md`, following `.github/pull_request_template.md`, and run `mise run lint-pr-description`. The `watch-pr`, `fix-pr` and `merge-pr` skills cover what follows.

## Markdown

Write each paragraph on one line, with no hard wrapping. `mise run lint-markdown-wrap` enforces it.

## Stash and work in progress

Agent sessions can run in parallel worktrees, and every worktree shares one global `git stash` stack.

- Use `git rebase --autostash` instead of a manual stash when you rebase a dirty worktree.
- Never run a bare `git stash pop` or `git stash apply`. Name a manual stash with `git stash push -m`, check `git stash list` right before restoring, and pop the explicit `stash@{n}`.
- To park work across a rebase or branch switch, commit it with `git commit -am wip --no-verify` and unwind it with `git reset --soft HEAD~1`. This throwaway commit is the one sanctioned use of `--no-verify`, and real commits never use it.
