// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

package nastools

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

const (
	// A shorter static file line would redact common text everywhere.
	minLine = 8
	// The largest config or input file read. The child environment can't
	// carry a larger value anyway.
	maxInput = 1 << 20
)

var errInput = errors.New("not a regular file within the size limit")

var (
	quoted  = regexp.MustCompile(`"([^"\\]*)"|'([^']*)'`)
	envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

type discovered struct {
	names     []string // Environment variables whose values are private.
	hosts     []string // Environment variables holding an address or known hosts.
	files     []string // Static files, private line by line.
	templates []string // Expansion templates.
}

// Private lists the values that Discover finds.
type Private struct {
	Values []string
	Hosts  []string // The address and known-hosts values, which hold host names.
}

// Discover lists every private value the config at path can bring into the
// output. It reads leniently, so a config that later fails validation still
// yields its values. It returns what it finds and doesn't write. Pass the
// ReadFile of Cached, which reads only regular files within a size limit.
func Discover(read func(string) ([]byte, error), path string, env map[string]string) Private {
	values := strings.Split(env["NASTOOLS_PRIVATE_STRINGS"], ",")
	for i := range values {
		values[i] = strings.TrimSpace(values[i])
	}
	data, err := read(path)
	if err != nil {
		return Private{Values: values}
	}
	found := decode(data, env)
	for _, name := range found.names {
		values = append(values, env[name])
	}
	hosts := make([]string, 0, len(found.hosts))
	for _, name := range found.hosts {
		hosts = append(hosts, env[name])
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
	return Private{Values: values, Hosts: hosts}
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
	var raw map[string]any
	if toml.Unmarshal(data, &raw) != nil {
		return scan(string(data), env)
	}
	doc := foldTable(raw)
	found := discovered{
		names:     texts(doc["required_env"]),
		files:     texts(doc["files"]),
		templates: texts(doc["expand_files"]),
	}
	if ssh, ok := doc["ssh"].(map[string]any); ok {
		for _, key := range slices.Sorted(maps.Keys(ssh)) {
			switch {
			case key == "address_env" || key == "known_hosts_env":
				found.hosts = append(found.hosts, texts(ssh[key])...)
			case strings.HasSuffix(key, "_env"):
				found.names = append(found.names, texts(ssh[key])...)
			}
		}
	}
	return found
}

// fold lower-cases every key and merges the keys that then collide. The
// strict decoder also lower-cases a key that matches no field exactly.
func fold(value any) any {
	switch v := value.(type) {
	case []any:
		result := make([]any, len(v))
		for i, item := range v {
			result[i] = fold(item)
		}
		return result
	case map[string]any:
		return foldTable(v)
	}
	return value
}

func foldTable(table map[string]any) map[string]any {
	result := make(map[string]any, len(table))
	for _, key := range slices.Sorted(maps.Keys(table)) {
		lower := strings.ToLower(key)
		result[lower] = merge(result[lower], fold(table[key]))
	}
	return result
}

// merge combines two values whose keys fold together. Tables merge key by
// key, and any other pair becomes a list holding both.
func merge(a, b any) any {
	if a == nil {
		return b
	}
	tableA, okA := a.(map[string]any)
	tableB, okB := b.(map[string]any)
	if !okA || !okB {
		return []any{a, b}
	}
	for _, key := range slices.Sorted(maps.Keys(tableB)) {
		tableA[key] = merge(tableA[key], tableB[key])
	}
	return tableA
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

// scan reads a config that fails to parse. It takes the quoted values of
// [files] and [expand_files], whether a table header, a dotted key, or an
// inline table holds them, and anywhere each quoted name the environment
// sets. Any of those names could be the address. Each yields host names.
func scan(data string, env map[string]string) discovered {
	var found discovered
	table := ""
	for line := range strings.Lines(data) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			header, _, _ := strings.Cut(line, "]")
			table = strings.Trim(header, "[")
			continue
		}
		for _, name := range quotedTexts(line) {
			if envName.MatchString(name) && env[name] != "" {
				found.hosts = append(found.hosts, name)
			}
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch topTable(table, key) {
		case "files":
			found.files = append(found.files, quotedTexts(value)...)
		case "expand_files":
			found.templates = append(found.templates, quotedTexts(value)...)
		}
	}
	return found
}

// topTable returns the top-level table that a key under a table header
// belongs to, in lower case.
func topTable(table, key string) string {
	path := strings.ToLower(strings.Join(strings.Fields(table+"."+key), ""))
	first, _, _ := strings.Cut(strings.TrimLeft(path, "."), ".")
	return strings.Trim(first, `"'`)
}

func quotedTexts(text string) []string {
	matches := quoted.FindAllStringSubmatch(text, -1)
	result := make([]string, 0, len(matches))
	for _, match := range matches {
		result = append(result, match[1]+match[2])
	}
	return result
}

// Parses reports whether the config at path reads and passes the strict
// decoder. Only then does Discover read every key the policy can use.
func Parses(read func(string) ([]byte, error), path string) bool {
	data, err := read(path)
	if err != nil {
		return false
	}
	_, err = parse(data)
	return err == nil
}

type cachedFile struct {
	data []byte
	err  error
}

type cached struct {
	Operations

	files map[string]cachedFile
}

// Cached reads each file once through ReadInput, so discovery and policy
// read the same bytes, and neither blocks on a declared device.
func Cached(ops Operations) Operations {
	return &cached{Operations: ops, files: make(map[string]cachedFile)}
}

func (c *cached) ReadFile(path string) ([]byte, error) {
	file, ok := c.files[path]
	if !ok {
		file.data, file.err = ReadInput(c.Operations, path)
		c.files[path] = file
	}
	return file.data, file.err
}

// ReadInput reads a regular file of at most 1 MiB. It checks the type before
// it opens the file, so a named pipe or a device such as /dev/zero can
// neither block the read nor fill memory.
func ReadInput(files Files, path string) ([]byte, error) {
	info, err := files.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat input: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errInput
	}
	file, err := files.OpenFile(path)
	if err != nil {
		return nil, fmt.Errorf("open input: %w", err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxInput+1))
	if err != nil || len(data) > maxInput {
		return nil, errInput
	}
	return data, nil
}
