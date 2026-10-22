// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package nastools

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// MinCompose is the oldest Compose release that nastools supports. The
// README names the same release.
const MinCompose = "2.23.1"

// composeVersion matches an optional v and three dot-separated numbers,
// then ignores anything after a - or +.
var composeVersion = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:[-+].*)?$`)

// shownVersion matches the probe output that a refusal may echo. The echo
// stays byte for byte as Compose printed it, so the redaction gate can still
// match a private value in it.
var shownVersion = regexp.MustCompile(`^[A-Za-z0-9.+_-]{1,40}$`)

// withoutEngine returns a copy of the environment without the variables
// that select a Docker engine.
func withoutEngine(env []string) []string {
	return slices.DeleteFunc(slices.Clone(env), func(entry string) bool {
		name, _, _ := strings.Cut(entry, "=")
		return slices.Contains(
			[]string{"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_TLS", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"},
			name,
		)
	})
}

// parseVersion returns the numeric parts of a Compose version.
func parseVersion(text string) ([]int, bool) {
	match := composeVersion.FindStringSubmatch(text)
	if match == nil {
		return nil, false
	}
	version := make([]int, 0, len(match)-1)
	for _, part := range match[1:] {
		number, err := strconv.Atoi(part)
		if err != nil {
			return nil, false
		}
		version = append(version, number)
	}
	return version, true
}

// checkCompose runs before any other Compose call and refuses a local
// Compose older than MinCompose. A refusal leaves out the adapter's text and
// echoes the first line that Compose printed only when that line is short
// and uses only characters that a version can contain.
func checkCompose(ctx context.Context, ops Operations, env []string) error {
	var out bytes.Buffer
	if ops.Compose(ctx, []string{"version", "--short"}, withoutEngine(env), &out, nil) != nil {
		return fmt.Errorf("compose version probe: %w", ErrInvalid)
	}
	line, _, _ := strings.Cut(out.String(), "\n")
	line = strings.TrimSpace(line)
	version, ok := parseVersion(line)
	minimum, _ := parseVersion(MinCompose)
	if ok && slices.Compare(version, minimum) >= 0 {
		return nil
	}
	read := "unparsable output"
	if shownVersion.MatchString(line) {
		read = "read " + line
	}
	return fmt.Errorf("compose version: %s, need %s or newer: %w", read, MinCompose, ErrInvalid)
}
