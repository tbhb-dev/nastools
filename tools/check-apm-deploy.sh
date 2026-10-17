#!/usr/bin/env bash
# The frozen-consumer check for the bare devtools APM pin (D13).
#
# apm.yml pins the bare tbhb-dev/devtools#vX.Y.Z and narrows it with
# `target: [claude, codex]`. The devtools README says target
# intersection then deploys the claude, common, and codex packages and
# skips the antigravity scaffold. This check doesn't assume it. It fails
# when:
#
#   * the deployed-file set in apm.lock.yaml differs from the recorded
#     set in tools/apm-deploy.txt
#   * a recorded file is missing from the tree
#   * anything lands in an antigravity location, or the agy package
#     deploys a file
#   * a Claude skill lacks its Codex twin under .agents/skills/
#
# After a deliberate pin move, review the new set and record it with
# `bash tools/check-apm-deploy.sh --record`.
set -euo pipefail

recorded=tools/apm-deploy.txt

# Print every deployed_files entry in apm.lock.yaml as
# "<package name> <path>", sorted.
deployed() {
  awk '
    /^- / { pkg = ""; in_files = 0 }
    /^[- ] name:/ { pkg = $NF }
    /^  deployed_files:/ { in_files = 1; next }
    in_files && /^  - / { print pkg, $2; next }
    in_files && !/^  - / { in_files = 0 }
  ' apm.lock.yaml | LC_ALL=C sort
}

if [[ ${1:-} == --record ]]; then
  deployed >"$recorded"
  # shellcheck disable=SC2312 # the count is only reported
  echo "recorded $(wc -l <"$recorded") deployed files in $recorded"
  exit 0
fi

current=$(deployed)
if [[ -z $current ]]; then
  echo "apm.lock.yaml lists no deployed files, so this check verified nothing" >&2
  exit 1
fi

rc=0

if ! diff -u --label "$recorded" --label apm.lock.yaml "$recorded" - <<<"$current"; then
  echo "the APM deploy set changed; review it, then run 'bash tools/check-apm-deploy.sh --record'" >&2
  rc=1
fi

while read -r _ path; do
  if [[ ! -e $path ]]; then
    echo "recorded APM file is missing from the tree: $path" >&2
    rc=1
  fi
done <"$recorded"

agy=$(awk '$1 == "devtools-agents-agy"' <<<"$current")
if [[ -n $agy ]]; then
  echo "the antigravity package deployed files:" >&2
  printf '  %s\n' "$agy" >&2
  rc=1
fi

for dir in .agent .agents/rules .agents/workflows .gemini; do
  if [[ -e $dir ]]; then
    echo "found an antigravity location: $dir" >&2
    rc=1
  fi
done

shopt -s nullglob
skills=(.claude/skills/*/)
if [[ ${#skills[@]} -eq 0 ]]; then
  echo "no Claude skills deployed, so the Codex comparison checked nothing" >&2
  rc=1
fi
for skill in "${skills[@]}"; do
  name=$(basename "$skill")
  if [[ ! -f .agents/skills/$name/SKILL.md ]]; then
    echo "Claude skill $name has no Codex twin at .agents/skills/$name" >&2
    rc=1
  fi
done

if [[ $rc -eq 0 ]]; then
  # shellcheck disable=SC2312 # the count is only reported
  echo "APM deploy matches the recorded frozen-consumer set ($(wc -l <"$recorded") files, ${#skills[@]} skills per harness)"
fi
exit "$rc"
