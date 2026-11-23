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

package cmd

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"gopkg.in/yaml.v3"
)

const (
	secretsFileFlagName  = "secrets-file"
	secretsFileFlagUsage = "Path to a YAML/JSON file containing agent-scoped secrets (read client-side)"

	// maxSecretsFileValueBytes matches the hub's 64 KiB per-secret value limit.
	maxSecretsFileValueBytes = 64 * 1024
)

var secretsFilePath string

type secretsFileEntry struct {
	Key           string `yaml:"key"`
	Value         string `yaml:"value"`
	ValueFile     string `yaml:"valueFile"`
	FromEnv       string `yaml:"fromEnv"`
	Type          string `yaml:"type"`
	Target        string `yaml:"target"`
	InjectionMode string `yaml:"injectionMode"`
	Description   string `yaml:"description"`
}

type secretsFileFull struct {
	Secrets []secretsFileEntry `yaml:"secrets"`
}

// loadSecretsFile reads and parses a --secrets-file YAML/JSON file in either
// map shorthand (`KEY: value`) or full form (`secrets: [{key, value|valueFile|fromEnv, ...}]`).
// On Unix, it writes a warning to warnOut if the file is group- or world-readable.
func loadSecretsFile(path string, warnOut io.Writer) ([]hubclient.AgentSecretInput, error) {
	if path == "" {
		return nil, nil
	}
	expanded, err := expandSecretsPath(path)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve secrets file path %q: %w", path, err)
	}
	if warnOut != nil {
		warnIfSecretsFileOverlyPermissive(warnOut, expanded)
	}
	data, err := os.ReadFile(expanded)
	if err != nil {
		return nil, fmt.Errorf("failed to read secrets file %q: %w", path, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, fmt.Errorf("secrets file %q is empty", path)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("failed to parse secrets file %q: %w", path, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("secrets file %q must be a YAML/JSON mapping", path)
	}
	root := doc.Content[0]
	if len(root.Content) == 0 {
		return nil, fmt.Errorf("secrets file %q contains no secrets", path)
	}

	// Detect full form: top-level "secrets" key holding a sequence.
	isFullForm := false
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "secrets" && root.Content[i+1].Kind == yaml.SequenceNode {
			isFullForm = true
			break
		}
	}

	if isFullForm {
		if len(root.Content) != 2 {
			return nil, fmt.Errorf("secrets file %q in full form must only contain the top-level 'secrets' list", path)
		}
		var full secretsFileFull
		if err := root.Decode(&full); err != nil {
			return nil, fmt.Errorf("failed to parse secrets list in %q: %w", path, err)
		}
		if len(full.Secrets) == 0 {
			return nil, fmt.Errorf("secrets file %q contains no secrets", path)
		}
		baseDir := filepath.Dir(expanded)
		out := make([]hubclient.AgentSecretInput, 0, len(full.Secrets))
		for i, entry := range full.Secrets {
			sec, err := resolveSecretsFileEntry(entry, i, baseDir)
			if err != nil {
				return nil, fmt.Errorf("secrets file %q: %w", path, err)
			}
			out = append(out, sec)
		}
		return out, nil
	}

	// Map shorthand: KEY: value (environment, as_needed).
	var kv map[string]string
	if err := root.Decode(&kv); err != nil {
		return nil, fmt.Errorf("failed to parse secrets map in %q: %w", path, err)
	}
	if len(kv) == 0 {
		return nil, fmt.Errorf("secrets file %q contains no secrets", path)
	}
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]hubclient.AgentSecretInput, 0, len(keys))
	for _, k := range keys {
		v := kv[k]
		if v == "" {
			return nil, fmt.Errorf("secrets file %q: secret %q has an empty value", path, k)
		}
		if len(v) > maxSecretsFileValueBytes {
			return nil, fmt.Errorf("secrets file %q: secret %q exceeds 64KB limit", path, k)
		}
		out = append(out, hubclient.AgentSecretInput{
			Key:      k,
			Value:    v,
			Encoding: "raw",
		})
	}
	return out, nil
}

func resolveSecretsFileEntry(entry secretsFileEntry, index int, baseDir string) (hubclient.AgentSecretInput, error) {
	if strings.TrimSpace(entry.Key) == "" {
		return hubclient.AgentSecretInput{}, fmt.Errorf("secrets[%d]: key is required", index)
	}

	sources := 0
	if entry.Value != "" {
		sources++
	}
	if entry.ValueFile != "" {
		sources++
	}
	if entry.FromEnv != "" {
		sources++
	}
	if sources == 0 {
		return hubclient.AgentSecretInput{}, fmt.Errorf("secret %q: one of value, valueFile, or fromEnv is required", entry.Key)
	}
	if sources > 1 {
		return hubclient.AgentSecretInput{}, fmt.Errorf("secret %q: only one of value, valueFile, or fromEnv may be set", entry.Key)
	}

	var value, encoding string
	switch {
	case entry.Value != "":
		if len(entry.Value) > maxSecretsFileValueBytes {
			return hubclient.AgentSecretInput{}, fmt.Errorf("secret %q: value exceeds 64KB limit", entry.Key)
		}
		value = entry.Value
		encoding = "raw"

	case entry.ValueFile != "":
		vp, err := expandSecretsPath(entry.ValueFile)
		if err != nil {
			return hubclient.AgentSecretInput{}, fmt.Errorf("secret %q: failed to resolve valueFile %q: %w", entry.Key, entry.ValueFile, err)
		}
		if !filepath.IsAbs(vp) {
			vp = filepath.Join(baseDir, vp)
		}
		data, err := os.ReadFile(vp)
		if err != nil {
			return hubclient.AgentSecretInput{}, fmt.Errorf("secret %q: failed to read valueFile %q: %w", entry.Key, entry.ValueFile, err)
		}
		if len(data) == 0 {
			return hubclient.AgentSecretInput{}, fmt.Errorf("secret %q: valueFile %q is empty", entry.Key, entry.ValueFile)
		}
		if len(data) > maxSecretsFileValueBytes {
			return hubclient.AgentSecretInput{}, fmt.Errorf("secret %q: valueFile %q exceeds 64KB limit", entry.Key, entry.ValueFile)
		}
		value = base64.StdEncoding.EncodeToString(data)
		encoding = "base64"

	case entry.FromEnv != "":
		envVal, ok := os.LookupEnv(entry.FromEnv)
		if !ok || envVal == "" {
			return hubclient.AgentSecretInput{}, fmt.Errorf("secret %q: environment variable %q is not set or empty", entry.Key, entry.FromEnv)
		}
		if len(envVal) > maxSecretsFileValueBytes {
			return hubclient.AgentSecretInput{}, fmt.Errorf("secret %q: value from env %q exceeds 64KB limit", entry.Key, entry.FromEnv)
		}
		value = envVal
		encoding = "raw"
	}

	return hubclient.AgentSecretInput{
		Key:           entry.Key,
		Value:         value,
		Encoding:      encoding,
		Type:          entry.Type,
		Target:        entry.Target,
		InjectionMode: entry.InjectionMode,
		Description:   entry.Description,
	}, nil
}

func expandSecretsPath(p string) (string, error) {
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, p[2:]), nil
	}
	return p, nil
}
