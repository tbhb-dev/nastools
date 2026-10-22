#!/bin/sh
# Wraps flock in the SSH engine fixture, installed as /usr/local/bin/flock.
# It logs each lock attempt of the form that nastools makes, `flock -n
# <lock> <command>`, as one line of event and lock: busy when the lock is
# refused, and acquired and released around the command. The command runs
# through this wrapper with --held, so those two lines are written while the
# lock is held, and their order in the log is the order in which deploys
# held the lock. Any other form passes straight to flock.
set -eu

log=/var/lib/nastools/flock.log

if [ "$#" -eq 4 ] && [ "$1" = --held ]; then
  touch "$4/acquired"
  echo "acquired $2" >>"$log"
  status=0
  "$3" || status=$?
  echo "released $2" >>"$log"
  exit "$status"
fi

if [ "$#" -ne 3 ] || [ "$1" != -n ]; then
  exec /usr/bin/flock "$@"
fi

# The held wrapper creates a private marker before running the command.
# Without that marker, status 75 is a refusal, returned as 1 to the caller,
# as flock itself would. A held command can return any status, including 75.
marker=$(mktemp -d "${TMPDIR:-/tmp}/nastools-flock.XXXXXX")
trap 'if [ -e "$marker/acquired" ]; then rm "$marker/acquired"; fi; rmdir "$marker"' EXIT
status=0
/usr/bin/flock -n -E 75 "$2" /usr/local/bin/flock --held "$2" "$3" "$marker" || status=$?
if [ "$status" -eq 75 ] && [ ! -e "$marker/acquired" ]; then
  echo "busy $2" >>"$log"
  exit 1
fi
exit "$status"
