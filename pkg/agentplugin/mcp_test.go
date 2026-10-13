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
	"os"
	"strings"
	"testing"
)

const mcpSchema = `"$schema": "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json"`

func loadMCPFiles(t *testing.T, files map[string]string) (*MCPConfig, []Diagnostic) {
	t.Helper()
	dir := t.TempDir()
	writeFiles(t, dir, files)
	return LoadMCP(openRoot(t, dir), PluginSchemaV1)
}

func TestLoadMCPAbsent(t *testing.T) {
	cfg, diags := loadMCPFiles(t, nil)
	if cfg != nil || diags != nil {
		t.Fatalf("got %v %v", cfg, diags)
	}
}

func TestLoadMCPWholeFileDisabled(t *testing.T) {
	cases := map[string]string{
		"bad json":          `{`,
		"not object":        `[]`,
		"schema mismatch":   `{"$schema": "https://agent-plugins.org/schemas/1.1.0/mcp.schema.json", "mcpServers": {}}`,
		"missing schema":    `{"mcpServers": {}}`,
		"extra top level":   `{` + mcpSchema + `, "mcpServers": {}, "x": 1}`,
		"servers not obj":   `{` + mcpSchema + `, "mcpServers": []}`,
		"servers missing":   `{` + mcpSchema + `}`,
		"plugin.json in it": `{` + mcpSchema + `, "mcpServers": {}, "name": "x"}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			cfg, diags := loadMCPFiles(t, map[string]string{"mcp.json": content})
			if cfg != nil || len(diags) != 1 || !strings.HasPrefix(diags[0].Message, "MCP disabled: ") {
				t.Fatalf("got %v %v", cfg, diags)
			}
		})
	}
	t.Run("manifest targets other version", func(t *testing.T) {
		dir := t.TempDir()
		writeFiles(t, dir, map[string]string{"mcp.json": `{` + mcpSchema + `, "mcpServers": {}}`})
		cfg, diags := LoadMCP(openRoot(t, dir), "https://agent-plugins.org/schemas/1.1.0/plugin.schema.json")
		if cfg != nil || len(diags) != 1 {
			t.Fatalf("got %v %v", cfg, diags)
		}
	})
	t.Run("directory", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(dir+"/mcp.json", 0o755); err != nil {
			t.Fatal(err)
		}
		cfg, diags := LoadMCP(openRoot(t, dir), PluginSchemaV1)
		if cfg != nil || len(diags) != 1 {
			t.Fatalf("got %v %v", cfg, diags)
		}
	})
}

func TestLoadMCPEmptyServers(t *testing.T) {
	cfg, diags := loadMCPFiles(t, map[string]string{"mcp.json": `{` + mcpSchema + `, "mcpServers": {}}`})
	if cfg == nil || cfg.Valid == nil || len(cfg.Valid) != 0 || len(diags) != 0 {
		t.Fatalf("got %+v %v", cfg, diags)
	}
}

func TestLoadMCPPerServer(t *testing.T) {
	servers := map[string]struct {
		json  string
		valid bool
	}{
		"bare":              {`{"type": "stdio", "command": "npx", "args": ["-y", "${PLUGIN_ROOT}/x"], "env": {"A": "${PLUGIN_DATA}"}}`, true},
		"local-cmd":         {`{"type": "stdio", "command": "./bin/srv"}`, true},
		"foreign-field":     {`{"type": "stdio", "command": "npx", "url": "https://x.example"}`, false},
		"unknown-field":     {`{"type": "stdio", "command": "npx", "shell": true}`, false},
		"unknown-type":      {`{"type": "websocket", "url": "https://x.example"}`, false},
		"missing-type":      {`{"command": "npx"}`, false},
		"cmd-whitespace":    {`{"type": "stdio", "command": "npx -y srv"}`, false},
		"cmd-escape":        {`{"type": "stdio", "command": "../x"}`, false},
		"cmd-dotdot-local":  {`{"type": "stdio", "command": "./../x"}`, false},
		"cmd-abs":           {`{"type": "stdio", "command": "/usr/bin/x"}`, false},
		"cmd-dir":           {`{"type": "stdio", "command": "./bin"}`, false},
		"cmd-missing":       {`{"type": "stdio", "command": "./bin/nope"}`, false},
		"cmd-empty":         {`{"type": "stdio", "command": ""}`, false},
		"cwd-bare":          {`{"type": "stdio", "command": "npx", "cwd": "data"}`, false},
		"cwd-local":         {`{"type": "stdio", "command": "npx", "cwd": "./d"}`, true},
		"cwd-root":          {`{"type": "stdio", "command": "npx", "cwd": "${PLUGIN_ROOT}"}`, true},
		"cwd-root-escape":   {`{"type": "stdio", "command": "npx", "cwd": "${PLUGIN_ROOT}/../x"}`, false},
		"cwd-data-sub":      {`{"type": "stdio", "command": "npx", "cwd": "${PLUGIN_DATA}/x"}`, true},
		"cwd-data-escape":   {`{"type": "stdio", "command": "npx", "cwd": "${PLUGIN_DATA}/../x"}`, false},
		"cwd-rootish":       {`{"type": "stdio", "command": "npx", "cwd": "${PLUGIN_ROOT}x"}`, false},
		"env-reserved-root": {`{"type": "stdio", "command": "npx", "env": {"PLUGIN_ROOT": "/x"}}`, false},
		"env-reserved-data": {`{"type": "stdio", "command": "npx", "env": {"PLUGIN_DATA": "/x"}}`, false},
		"env-not-string":    {`{"type": "stdio", "command": "npx", "env": {"A": 1}}`, false},
		"args-not-strings":  {`{"type": "stdio", "command": "npx", "args": [1]}`, false},
		"http-plain":        {`{"type": "streamable-http", "url": "http://example.com/mcp"}`, false},
		"http-localhost":    {`{"type": "streamable-http", "url": "http://localhost:8765/mcp"}`, true},
		"http-loopback":     {`{"type": "streamable-http", "url": "http://127.0.0.1:8765/mcp"}`, true},
		"http-loopback-v6":  {`{"type": "sse", "url": "http://[::1]:8765/sse"}`, true},
		"https":             {`{"type": "sse", "url": "https://example.com/sse", "headers": {"X-Tenant": "t"}}`, true},
		"userinfo":          {`{"type": "streamable-http", "url": "https://u:p@example.com/mcp"}`, false},
		"fragment":          {`{"type": "streamable-http", "url": "https://example.com/mcp#x"}`, false},
		"relative-url":      {`{"type": "streamable-http", "url": "/mcp"}`, false},
		"ftp":               {`{"type": "streamable-http", "url": "ftp://example.com/mcp"}`, false},
		"dup-headers":       {`{"type": "streamable-http", "url": "https://example.com", "headers": {"X-A": "1", "x-a": "2"}}`, false},
		"bad-header-name":   {`{"type": "streamable-http", "url": "https://example.com", "headers": {"X A": "1"}}`, false},
		"http-stdio-field":  {`{"type": "streamable-http", "url": "https://example.com", "command": "x"}`, false},
	}
	var b strings.Builder
	b.WriteString(`{` + mcpSchema + `, "mcpServers": {`)
	first := true
	for n, s := range servers {
		if !first {
			b.WriteString(",")
		}
		first = false
		b.WriteString(`"` + n + `": ` + s.json)
	}
	b.WriteString(`}}`)
	cfg, diags := loadMCPFiles(t, map[string]string{
		"mcp.json":      b.String(),
		"bin/srv*":      "#!/bin/sh\n",
		"bin/sub/other": "x",
	})
	if cfg == nil {
		t.Fatalf("whole file disabled: %v", diags)
	}
	if len(diags) != len(cfg.Invalid) {
		t.Errorf("diags %d != invalid %d", len(diags), len(cfg.Invalid))
	}
	invalid := map[string]string{}
	for _, d := range cfg.Invalid {
		for n := range servers {
			if strings.HasPrefix(d.Message, `mcp server "`+n+`" skipped: `) {
				invalid[n] = d.Message
			}
		}
	}
	for n, s := range servers {
		_, isValid := cfg.Valid[n]
		_, isInvalid := invalid[n]
		if isValid == isInvalid {
			t.Errorf("server %s: valid=%v invalid=%v", n, isValid, isInvalid)
		}
		if isValid != s.valid {
			t.Errorf("server %s: valid=%v, want %v (%s)", n, isValid, s.valid, invalid[n])
		}
	}
	if got := cfg.Valid["bare"]; got.Command != "npx" || len(got.Args) != 2 || got.Env["A"] != "${PLUGIN_DATA}" {
		t.Errorf("bare server = %+v", got)
	}
}

func TestLoadMCPCommandSymlink(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"mcp.json":  `{` + mcpSchema + `, "mcpServers": {"s": {"type": "stdio", "command": "./bin/link"}, "t": {"type": "stdio", "command": "./lnk/real"}}}`,
		"bin/real*": "#!/bin/sh\n",
	})
	if err := os.Symlink("real", dir+"/bin/link"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("bin", dir+"/lnk"); err != nil {
		t.Fatal(err)
	}
	cfg, _ := LoadMCP(openRoot(t, dir), PluginSchemaV1)
	if cfg == nil || len(cfg.Valid) != 0 || len(cfg.Invalid) != 2 {
		t.Fatalf("got %+v", cfg)
	}
}
