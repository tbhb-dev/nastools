// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package nastools

import (
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// A shorter static file line would redact common text everywhere.
const minLine = 8

var (
	quoted  = regexp.MustCompile(`"([^"\\]*)"|'([^']*)'`)
	envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

type discovered struct {
	names     []string // Environment variables whose values are private.
	files     []string // Static files, private line by line.
	templates []string // Expansion templates.
}

// Discover lists every private value the config at path can bring into the
// output. It reads leniently, so a config that later fails validation still
// yields its values. It returns what it finds and doesn't write.
func Discover(read func(string) ([]byte, error), path string, env map[string]string) []string {
	values := strings.Split(env["NASTOOLS_PRIVATE_STRINGS"], ",")
	for i := range values {
		values[i] = strings.TrimSpace(values[i])
	}
	data, err := read(path)
	if err != nil {
		return values
	}
	found := decode(data, env)
	for _, name := range found.names {
		values = append(values, env[name])
	}
	dir := filepath.Dir(path)
	for _, template := range found.templates {
		// Prepare expands with os.Expand too, so both find the same references.
		os.Expand(readText(read, dir, template), func(key string) string {
			values = append(values, env[key])
			return ""
		})
	}
	for _, file := range found.files {
		for line := range strings.Lines(readText(read, dir, file)) {
			if line = strings.TrimSpace(line); len(line) >= minLine {
				values = append(values, line)
			}
		}
	}
	return values
}

// readText returns the file at path, or an empty string for an unreadable file.
func readText(read func(string) ([]byte, error), dir, path string) string {
	data, err := read(resolve(dir, path))
	if err != nil {
		return ""
	}
	return string(data)
}

// decode reads the config without its schema, falling back to scan on a
// syntax error.
func decode(data []byte, env map[string]string) discovered {
	var doc map[string]any
	if toml.Unmarshal(data, &doc) != nil {
		return scan(string(data), env)
	}
	found := discovered{
		names:     texts(doc["required_env"]),
		files:     texts(doc["files"]),
		templates: texts(doc["expand_files"]),
	}
	if ssh, ok := doc["ssh"].(map[string]any); ok {
		for _, key := range slices.Sorted(maps.Keys(ssh)) {
			if strings.HasSuffix(key, "_env") {
				found.names = append(found.names, texts(ssh[key])...)
			}
		}
	}
	return found
}

// texts collects every string in a decoded config value.
func texts(value any) []string {
	var result []string
	switch v := value.(type) {
	case string:
		result = append(result, v)
	case []any:
		for _, item := range v {
			result = append(result, texts(item)...)
		}
	case map[string]any:
		for _, key := range slices.Sorted(maps.Keys(v)) {
			result = append(result, texts(v[key])...)
		}
	}
	return result
}

// scan reads a config that fails to parse. It takes the quoted values under
// [files] and [expand_files], and anywhere each quoted name the environment sets.
func scan(data string, env map[string]string) discovered {
	var found discovered
	table := ""
	for line := range strings.Lines(data) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			header, _, _ := strings.Cut(line, "]")
			table = strings.TrimSpace(strings.Trim(header, "["))
			continue
		}
		for _, match := range quoted.FindAllStringSubmatch(line, -1) {
			if name := match[1] + match[2]; envName.MatchString(name) && env[name] != "" {
				found.names = append(found.names, name)
			}
		}
		_, value, ok := strings.Cut(line, "=")
		match := quoted.FindStringSubmatch(value)
		if !ok || match == nil {
			continue
		}
		switch table {
		case "files":
			found.files = append(found.files, match[1]+match[2])
		case "expand_files":
			found.templates = append(found.templates, match[1]+match[2])
		}
	}
	return found
}

type cachedFile struct {
	data []byte
	err  error
}

type cached struct {
	Operations

	files map[string]cachedFile
}

// Cached caches ReadFile results, so discovery and policy read the same bytes.
func Cached(ops Operations) Operations {
	return &cached{Operations: ops, files: make(map[string]cachedFile)}
}

func (c *cached) ReadFile(path string) ([]byte, error) {
	file, ok := c.files[path]
	if !ok {
		file.data, file.err = c.Operations.ReadFile(path)
		c.files[path] = file
	}
	return file.data, file.err
}
