// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package agentplugin

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"golang.org/x/net/http/httpguts"
)

// MCP transport types (spec §7.2.1).
const (
	MCPTypeStdio          = "stdio"
	MCPTypeStreamableHTTP = "streamable-http"
	MCPTypeSSE            = "sse"
)

const (
	placeholderRoot = "${PLUGIN_ROOT}"
	placeholderData = "${PLUGIN_DATA}"
)

// MCPServer is one validated mcp.json server entry.
type MCPServer struct {
	Type    string            `json:"type"` // stdio | streamable-http | sse
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// MCPConfig is a loaded mcp.json: the valid servers and a diagnostic for
// each skipped one.
type MCPConfig struct {
	Valid   map[string]MCPServer `json:"servers"`
	Invalid []Diagnostic         `json:"invalid,omitempty"`
}

// LoadMCP loads mcp.json from root. It returns (nil, nil) when mcp.json is
// absent. A whole-file failure (not a regular file, not JSON, wrong or
// mismatched $schema, extra top-level fields, mcpServers missing or not an
// object) returns (nil, [warning "MCP disabled: ..."]) per spec §7.2.2(2).
// A per-server failure adds an entry to Invalid and keeps the other servers
// (§7.2.2(3)); those entries are also returned as diagnostics.
//
// Per-server rules: each variant is closed (an unknown field or a field of
// another variant is invalid). stdio: command non-empty, a single token, a
// bare name or a "./" path naming a regular file inside root (no symlink);
// cwd absent, "./...", "${PLUGIN_ROOT}[/...]" or "${PLUGIN_DATA}[/...]" and
// lexically contained; env keys other than PLUGIN_ROOT and PLUGIN_DATA.
// http/sse: an absolute http(s) URL with no userinfo or fragment, https
// unless the host is exactly "localhost" or a loopback IP literal; header
// names and values valid, no case-insensitive duplicate names.
func LoadMCP(root *os.Root, manifestSchema string) (*MCPConfig, []Diagnostic) {
	fi, err := root.Lstat(MCPFile)
	if os.IsNotExist(err) {
		return nil, nil
	}
	disabled := func(format string, args ...any) (*MCPConfig, []Diagnostic) {
		return nil, []Diagnostic{warning("MCP disabled: "+format, args...)}
	}
	if err != nil {
		return disabled("%v", err)
	}
	if !fi.Mode().IsRegular() {
		return disabled("%s is not a regular file", MCPFile)
	}
	data, err := readRegularFile(root, MCPFile, maxJSONFileSize)
	if err != nil {
		return disabled("%v", err)
	}
	members, err := decodeObject(data)
	if err != nil {
		return disabled("%s: %v", MCPFile, err)
	}
	schema, ok := jsonString(members["$schema"])
	switch {
	case !ok:
		return disabled("%s: missing or non-string \"$schema\"", MCPFile)
	case schema != MCPSchemaV1:
		return disabled("%s: unsupported Agent Plugins version %q; Scion supports %s", MCPFile, schema, SpecVersion)
	case manifestSchema != PluginSchemaV1:
		return disabled("%s targets a different Agent Plugins version than %s", MCPFile, ManifestFile)
	}
	for k := range members {
		if k != "$schema" && k != "mcpServers" {
			return disabled("%s: unknown top-level field %q", MCPFile, k)
		}
	}
	rawServers, ok := members["mcpServers"]
	if !ok || !isJSONKind(rawServers, '{') {
		return disabled("%s: \"mcpServers\" must be an object", MCPFile)
	}
	var servers map[string]json.RawMessage
	if err := json.Unmarshal(rawServers, &servers); err != nil {
		return disabled("%s: \"mcpServers\" must be an object", MCPFile)
	}

	cfg := &MCPConfig{Valid: map[string]MCPServer{}}
	names := make([]string, 0, len(servers))
	for n := range servers {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		srv, err := validateServer(root, servers[n])
		if err != nil {
			cfg.Invalid = append(cfg.Invalid, warning("mcp server %q skipped: %v", n, err))
			continue
		}
		cfg.Valid[n] = srv
	}
	return cfg, slices.Clone(cfg.Invalid)
}

var variantFields = map[string][]string{
	MCPTypeStdio:          {"type", "command", "args", "env", "cwd"},
	MCPTypeStreamableHTTP: {"type", "url", "headers"},
	MCPTypeSSE:            {"type", "url", "headers"},
}

func validateServer(root *os.Root, raw json.RawMessage) (MCPServer, error) {
	var srv MCPServer
	if !isJSONKind(raw, '{') {
		return srv, fmt.Errorf("not an object")
	}
	var f map[string]json.RawMessage
	if err := json.Unmarshal(raw, &f); err != nil {
		return srv, fmt.Errorf("not an object")
	}
	rawType, ok := f["type"]
	if !ok {
		return srv, fmt.Errorf("missing \"type\"")
	}
	typ, ok := jsonString(rawType)
	if !ok {
		return srv, fmt.Errorf("\"type\" must be a string")
	}
	allowed, ok := variantFields[typ]
	if !ok {
		return srv, fmt.Errorf("unknown type %q", typ)
	}
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if !slices.Contains(allowed, k) {
			return srv, fmt.Errorf("field %q is not allowed for type %q", k, typ)
		}
	}
	srv.Type = typ
	if typ == MCPTypeStdio {
		return srv, validateStdio(root, f, &srv)
	}
	return srv, validateRemote(f, &srv)
}

func validateStdio(root *os.Root, f map[string]json.RawMessage, srv *MCPServer) error {
	rawCmd, ok := f["command"]
	if !ok {
		return fmt.Errorf("missing \"command\"")
	}
	cmd, ok := jsonString(rawCmd)
	if !ok {
		return fmt.Errorf("\"command\" must be a string")
	}
	if err := validateCommand(root, cmd); err != nil {
		return err
	}
	srv.Command = cmd

	if raw, ok := f["args"]; ok {
		args, ok := jsonStringArray(raw)
		if !ok {
			return fmt.Errorf("\"args\" must be an array of strings")
		}
		for _, a := range args {
			if strings.ContainsRune(a, 0) {
				return fmt.Errorf("\"args\" contains a NUL character")
			}
		}
		srv.Args = args
	}
	if raw, ok := f["env"]; ok {
		env, ok := jsonStringMap(raw)
		if !ok {
			return fmt.Errorf("\"env\" must be an object of strings")
		}
		for k, v := range env {
			switch {
			case k == "PLUGIN_ROOT" || k == "PLUGIN_DATA":
				return fmt.Errorf("\"env\" must not set reserved variable %s", k)
			case k == "" || strings.ContainsAny(k, "=\x00"):
				return fmt.Errorf("\"env\" has an invalid variable name %q", k)
			case strings.ContainsRune(v, 0):
				return fmt.Errorf("\"env\" value for %s contains a NUL character", k)
			}
		}
		srv.Env = env
	}
	if raw, ok := f["cwd"]; ok {
		cwd, ok := jsonString(raw)
		if !ok {
			return fmt.Errorf("\"cwd\" must be a string")
		}
		if err := validateCwd(cwd); err != nil {
			return err
		}
		srv.Cwd = cwd
	}
	return nil
}

// validateCommand checks a stdio command: one token, either a bare
// executable name or a "./" path to a regular file inside root.
func validateCommand(root *os.Root, cmd string) error {
	if cmd == "" {
		return fmt.Errorf("\"command\" is empty")
	}
	if strings.IndexFunc(cmd, unicode.IsSpace) >= 0 || strings.ContainsRune(cmd, 0) {
		return fmt.Errorf("\"command\" %q must be a single executable token", cmd)
	}
	if !strings.HasPrefix(cmd, "./") {
		if strings.ContainsAny(cmd, `/\`) {
			return fmt.Errorf("\"command\" %q must be a bare name or a plugin-relative path starting with ./", cmd)
		}
		return nil
	}
	rel := strings.TrimPrefix(cmd, "./")
	if rel == "" || !filepath.IsLocal(rel) {
		return fmt.Errorf("\"command\" %q escapes the plugin root", cmd)
	}
	rel = filepath.Clean(rel)
	if dir := filepath.Dir(rel); dir != "." {
		ok, err := realDirs(root, dir)
		if err != nil || !ok {
			return fmt.Errorf("\"command\" %q not found in the plugin (or a parent is a symlink)", cmd)
		}
	}
	fi, err := root.Lstat(rel)
	if err != nil {
		return fmt.Errorf("\"command\" %q not found in the plugin", cmd)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("\"command\" %q is not a regular file", cmd)
	}
	return nil
}

// validateCwd checks the cwd forms of spec §7.2.1 and lexical containment
// after placeholder substitution. The PLUGIN_DATA form is checked lexically
// only: the data directory is harness-owned.
func validateCwd(cwd string) error {
	var rest string
	switch {
	case strings.HasPrefix(cwd, "./"):
		rest = strings.TrimPrefix(cwd, "./")
	case cwd == placeholderRoot || cwd == placeholderData:
		return nil
	case strings.HasPrefix(cwd, placeholderRoot+"/"):
		rest = strings.TrimPrefix(cwd, placeholderRoot+"/")
	case strings.HasPrefix(cwd, placeholderData+"/"):
		rest = strings.TrimPrefix(cwd, placeholderData+"/")
	default:
		return fmt.Errorf("\"cwd\" %q must start with ./, ${PLUGIN_ROOT} or ${PLUGIN_DATA}", cwd)
	}
	if strings.Contains(rest, placeholderRoot) || strings.Contains(rest, placeholderData) || strings.ContainsRune(rest, 0) {
		return fmt.Errorf("\"cwd\" %q has a placeholder after its root", cwd)
	}
	if rest == "" {
		return nil
	}
	if !filepath.IsLocal(rest) {
		return fmt.Errorf("\"cwd\" %q escapes its root", cwd)
	}
	return nil
}

func validateRemote(f map[string]json.RawMessage, srv *MCPServer) error {
	rawURL, ok := f["url"]
	if !ok {
		return fmt.Errorf("missing \"url\"")
	}
	s, ok := jsonString(rawURL)
	if !ok {
		return fmt.Errorf("\"url\" must be a string")
	}
	if err := validateURL(s); err != nil {
		return err
	}
	srv.URL = s
	if raw, ok := f["headers"]; ok {
		h, ok := jsonStringMap(raw)
		if !ok {
			return fmt.Errorf("\"headers\" must be an object of strings")
		}
		seen := map[string]string{}
		names := make([]string, 0, len(h))
		for k := range h {
			names = append(names, k)
		}
		slices.Sort(names)
		for _, k := range names {
			if !httpguts.ValidHeaderFieldName(k) {
				return fmt.Errorf("invalid header name %q", k)
			}
			if !httpguts.ValidHeaderFieldValue(h[k]) {
				return fmt.Errorf("invalid value for header %q", k)
			}
			lk := strings.ToLower(k)
			if prev, dup := seen[lk]; dup {
				return fmt.Errorf("header %q duplicates %q", k, prev)
			}
			seen[lk] = k
		}
		srv.Headers = h
	}
	return nil
}

func validateURL(s string) error {
	if s == "" {
		return fmt.Errorf("\"url\" is empty")
	}
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("\"url\" is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("\"url\" must be an absolute http or https URL")
	}
	if u.Host == "" || u.Opaque != "" {
		return fmt.Errorf("\"url\" must be an absolute http or https URL")
	}
	if u.User != nil {
		return fmt.Errorf("\"url\" must not contain user information")
	}
	if strings.Contains(s, "#") {
		return fmt.Errorf("\"url\" must not contain a fragment")
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return fmt.Errorf("\"url\" must use https unless the host is localhost or a loopback address")
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
