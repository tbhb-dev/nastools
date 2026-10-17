#!/usr/bin/env bash
# Print the devtools release tag the manifest pins, for example v0.11.0.
# Exactly one active bare devtools dependency entry (a list item that starts
# with the dependency) must exist, and its whole ref, after any inline
# comment, must be a plain vX.Y.Z tag. A commented older pin or a mention in
# another field never counts, and a prerelease or trailing text is rejected
# rather than truncated.
#
# Usage: devtools-pin.sh [manifest]   (default: apm.yml)
set -euo pipefail

manifest=${1:-apm.yml}
entries=$(grep -E '^[[:space:]]*-[[:space:]]+tbhb-dev/devtools#' "$manifest" || true)

if [[ -z $entries ]]; then
  echo "no active devtools pin found in $manifest" >&2
  exit 1
fi
# shellcheck disable=SC2312 # printf and wc over a variable don't fail
if [[ $(printf '%s\n' "$entries" | wc -l) -ne 1 ]]; then
  echo "expected one active devtools pin in $manifest, found several" >&2
  exit 1
fi

ref=$(printf '%s\n' "$entries" | sed -E 's/^[[:space:]]*-[[:space:]]+tbhb-dev\/devtools#//; s/[[:space:]]+#.*$//; s/[[:space:]]+$//')
if [[ ! $ref =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "devtools pin '$ref' in $manifest is not a plain vX.Y.Z tag" >&2
  exit 1
fi
printf '%s\n' "$ref"
