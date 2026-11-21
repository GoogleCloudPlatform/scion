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

package rebuildpreflight

import (
	"errors"
	"path/filepath"
	"strings"
)

// PreflightDBEnv is the environment variable carrying the hub's database DSN
// to an executor-invoked `scion server preflight` subprocess.
const PreflightDBEnv = "SCION_PREFLIGHT_DB"

// PreflightCommand builds the argv and environment for running the newly
// built binary's preflight subcommand. The DSN is never placed in argv: it
// is passed solely via SCION_PREFLIGHT_DB in env, replacing any inherited
// value.
func PreflightCommand(bin, brokerID, configPath string, environ []string, dsn string) (argv, env []string) {
	argv = []string{bin, "server", "preflight", "--format", "json", "--colocated-broker-id", brokerID}
	if configPath != "" {
		argv = append(argv, "--config", configPath)
	}
	prefix := PreflightDBEnv + "="
	env = make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		if strings.HasPrefix(kv, prefix) {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, prefix+dsn)
	return argv, env
}

// AbsSQLiteDSN resolves a relative SQLite database path against cwd,
// preserving any optional "file:" prefix and "?..." query string.
// ":memory:", empty strings, and already-absolute paths are returned
// unchanged.
func AbsSQLiteDSN(dsn, cwd string) (string, error) {
	if dsn == "" {
		return "", nil
	}
	hasPrefix := strings.HasPrefix(dsn, "file:")
	rest := dsn
	if hasPrefix {
		rest = rest[len("file:"):]
	}
	var query string
	if idx := strings.IndexByte(rest, '?'); idx >= 0 {
		query = rest[idx:]
		rest = rest[:idx]
	}
	if rest == "" || rest == ":memory:" || filepath.IsAbs(rest) {
		return dsn, nil
	}
	var abs string
	if cwd != "" {
		abs = filepath.Clean(filepath.Join(cwd, rest))
	} else {
		var err error
		abs, err = filepath.Abs(rest)
		if err != nil {
			return "", err
		}
	}
	if hasPrefix {
		return "file:" + abs + query, nil
	}
	return abs + query, nil
}

// AbsCheck verifies that a non-empty server configPath is absolute. The hub
// absolutizes --config at startup before chdir; a relative path reaching the
// executor is an internal error.
func AbsCheck(configPath string) error {
	if configPath == "" {
		return nil
	}
	if !filepath.IsAbs(configPath) {
		return errors.New("internal: the server config path is not absolute")
	}
	return nil
}
