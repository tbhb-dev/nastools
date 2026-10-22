#!/bin/sh
# A stand-in for the Tailscale command-line tool, installed in the SSH
# engine fixture as /usr/local/bin/tailscale. It records each serve
# invocation in a log as one tab-separated line of Service ID, port, and
# target, and answers `serve status --json` from that log with the latest
# target for each Service ID and port.
set -eu

log=/var/lib/nastools/serve.log

if [ "$*" = "serve status --json" ]; then
  touch "$log"
  exec jq --raw-input --null-input '
    reduce (inputs | split("\t")) as [$service, $port, $target]
      ({Services: {}}; .Services[$service][$port] = $target)
  ' "$log"
fi

if [ "$#" -eq 4 ] && [ "$1" = serve ]; then
  case "$2 $3" in
  "--service="*" --https="* | "--service="*" --tcp="*)
    printf '%s\t%s\t%s\n' "${2#--service=}" "${3#--}" "$4" >>"$log"
    exit 0
    ;;
  *) ;;
  esac
fi

echo "tailscale stub: unsupported arguments" >&2
exit 64
