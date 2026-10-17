#!/usr/bin/env bats
# The pin parser behind install-guard-markdown.

setup() {
  script="$BATS_TEST_DIRNAME/devtools-pin.sh"
  manifest=$(mktemp)
}

teardown() {
  rm -f "$manifest"
}

@test "the repository manifest yields its pinned tag" {
  run bash "$script" "$BATS_TEST_DIRNAME/../apm.yml"
  [ "$status" -eq 0 ]
  [[ $output =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]
}

@test "a commented older pin does not win" {
  printf '%s\n' '    # - tbhb-dev/devtools#v0.9.0' '    - tbhb-dev/devtools#v0.11.0' >"$manifest"
  run bash "$script" "$manifest"
  [ "$status" -eq 0 ]
  [ "$output" = v0.11.0 ]
}

@test "a prerelease ref is rejected, not truncated" {
  printf '%s\n' '    - tbhb-dev/devtools#v0.11.0-rc.1' >"$manifest"
  run bash "$script" "$manifest"
  [ "$status" -eq 1 ]
  [[ $output == *"not a plain vX.Y.Z tag"* ]]
}

@test "several active pins are rejected" {
  printf '%s\n' '    - tbhb-dev/devtools#v0.10.0' '    - tbhb-dev/devtools#v0.11.0' >"$manifest"
  run bash "$script" "$manifest"
  [ "$status" -eq 1 ]
}

@test "no active pin is rejected" {
  printf '%s\n' '    # - tbhb-dev/devtools#v0.10.0' >"$manifest"
  run bash "$script" "$manifest"
  [ "$status" -eq 1 ]
}

@test "a mention in another field is not a dependency" {
  printf '%s\n' 'description: "tbhb-dev/devtools#v0.9.0"' '    - other/pkg#v1.0.0' >"$manifest"
  run bash "$script" "$manifest"
  [ "$status" -eq 1 ]
  [[ $output == *"no active devtools pin"* ]]
}

@test "trailing text after the tag is rejected" {
  printf '%s\n' '    - tbhb-dev/devtools#v0.10.0"garbage' >"$manifest"
  run bash "$script" "$manifest"
  [ "$status" -eq 1 ]
  [[ $output == *"not a plain vX.Y.Z tag"* ]]
}

@test "an inline comment naming another pin is ignored" {
  printf '%s\n' '    - tbhb-dev/devtools#v0.11.0  # was tbhb-dev/devtools#v0.9.0' >"$manifest"
  run bash "$script" "$manifest"
  [ "$status" -eq 0 ]
  [ "$output" = v0.11.0 ]
}
