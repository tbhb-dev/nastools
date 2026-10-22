#!/bin/sh
# Starts sshd in the SSH engine fixture. `docker cp` keeps the owner that a
# key file had on the machine it came from, which sshd refuses, so both key
# files first pass to root with the modes sshd expects.
set -eu

host_key=/etc/ssh/ssh_host_ed25519_key
authorized_keys=/home/nastools/.ssh/authorized_keys

chown root:root "$host_key" "$authorized_keys"
chmod 600 "$host_key"
chmod 644 "$authorized_keys"
exec /usr/sbin/sshd -D -e
