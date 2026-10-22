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
// shows at most the first 40 characters that Compose printed.
func checkCompose(ctx context.Context, ops Operations, env []string) error {
	const maxShown = 40
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
	if shown := []rune(line); len(shown) > maxShown {
		line = string(shown[:maxShown])
	}
	return fmt.Errorf("compose version: read %q, need %s or newer: %w", line, MinCompose, ErrInvalid)
}
