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

package config

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"gopkg.in/yaml.v3"
)

const harnessConfigsDirName = "harness-configs"

// HarnessConfigDir represents a harness-config directory on disk.
// Located at ~/.scion/harness-configs/<name>/ or .scion/harness-configs/<name>/
type HarnessConfigDir struct {
	Name   string             // Directory name (e.g., "claude", "gemini-experimental")
	Path   string             // Absolute path to the directory
	Config HarnessConfigEntry // Parsed config.yaml content
}

// LoadHarnessConfigDir loads a harness-config from an on-disk directory.
func LoadHarnessConfigDir(dirPath string) (*HarnessConfigDir, error) {
	absPath, err := filepath.Abs(dirPath)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve path: %w", err)
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return nil, fmt.Errorf("harness-config directory not found: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("harness-config path is not a directory: %s", absPath)
	}

	configPath := filepath.Join(absPath, "config.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config.yaml: %w", err)
	}

	if validationErrors, err := ValidateHarnessConfig(data); err != nil {
		return nil, err
	} else if len(validationErrors) > 0 {
		return nil, fmt.Errorf("invalid config.yaml: %s", validationErrors[0].Error())
	}

	var entry HarnessConfigEntry
	if err := yaml.Unmarshal(data, &entry); err != nil {
		return nil, fmt.Errorf("failed to parse config.yaml: %w", err)
	}

	return &HarnessConfigDir{
		Name:   filepath.Base(absPath),
		Path:   absPath,
		Config: entry,
	}, nil
}

// FindHarnessConfigDir resolves a harness-config by name, checking template-level,
// grove-level, then global directories.
// Optional templatePaths specify template directories whose harness-configs/
// subdirectories are checked first (highest precedence), per the harness-agnostic
// template design (§3.4).
func FindHarnessConfigDir(name string, grovePath string, templatePaths ...string) (*HarnessConfigDir, error) {
	// Check template-level first (highest precedence)
	for _, tplPath := range templatePaths {
		tplHarnessConfigDir := filepath.Join(tplPath, harnessConfigsDirName, name)
		if info, err := os.Stat(tplHarnessConfigDir); err == nil && info.IsDir() {
			return LoadHarnessConfigDir(tplHarnessConfigDir)
		}
	}

	// Check grove-level
	if grovePath != "" {
		groveHarnessConfigDir := filepath.Join(grovePath, harnessConfigsDirName, name)
		if info, err := os.Stat(groveHarnessConfigDir); err == nil && info.IsDir() {
			return LoadHarnessConfigDir(groveHarnessConfigDir)
		}
	}

	// Check global directory
	globalDir, err := GetGlobalDir()
	if err == nil {
		globalHarnessConfigDir := filepath.Join(globalDir, harnessConfigsDirName, name)
		if info, err := os.Stat(globalHarnessConfigDir); err == nil && info.IsDir() {
			return LoadHarnessConfigDir(globalHarnessConfigDir)
		}
	}

	// The "generic" harness has no embedded files and therefore no on-disk
	// directory.  Return a synthetic entry so callers (e.g. template-sync
	// agents) can proceed without a physical harness-config dir.
	if name == "generic" {
		return &HarnessConfigDir{
			Name:   "generic",
			Config: HarnessConfigEntry{Harness: "generic", Image: "scion-base:latest", User: "scion"},
		}, nil
	}

	return nil, fmt.Errorf("harness-config %q not found", name)
}

// ListHarnessConfigDirs lists all available harness-configs.
// Grove-level configs take precedence over global configs with the same name.
func ListHarnessConfigDirs(grovePath string) ([]*HarnessConfigDir, error) {
	seen := make(map[string]*HarnessConfigDir)

	// Load global configs first (lower precedence)
	globalDir, err := GetGlobalDir()
	if err == nil {
		loadHarnessConfigsFromDir(filepath.Join(globalDir, harnessConfigsDirName), seen)
	}

	// Load grove-level configs (higher precedence, overwrites global)
	if grovePath != "" {
		loadHarnessConfigsFromDir(filepath.Join(grovePath, harnessConfigsDirName), seen)
	}

	// Sort by name for deterministic output
	result := make([]*HarnessConfigDir, 0, len(seen))
	for _, hc := range seen {
		result = append(result, hc)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})

	return result, nil
}

// loadHarnessConfigsFromDir loads all harness-config directories from a parent dir.
func loadHarnessConfigsFromDir(parentDir string, into map[string]*HarnessConfigDir) {
	entries, err := os.ReadDir(parentDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		hc, err := LoadHarnessConfigDir(filepath.Join(parentDir, entry.Name()))
		if err != nil {
			continue
		}
		into[hc.Name] = hc
	}
}

// SeedHarnessConfig populates a harness-config directory from embedded defaults.
// targetDir is e.g. ~/.scion/harness-configs/claude/
func SeedHarnessConfig(targetDir string, h api.Harness, force bool) error {
	embedsFS, basePath := h.GetHarnessEmbedsFS()
	if basePath == "" {
		// Generic harness has no embeds
		return nil
	}

	homeDir := filepath.Join(targetDir, "home")

	// Create target directories
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return fmt.Errorf("failed to create harness-config directory: %w", err)
	}
	if err := os.MkdirAll(homeDir, 0755); err != nil {
		return fmt.Errorf("failed to create harness-config home directory: %w", err)
	}

	// Create config dir inside home if the harness specifies one
	configDir := h.DefaultConfigDir()
	if configDir != "" {
		if err := os.MkdirAll(filepath.Join(homeDir, configDir), 0755); err != nil {
			return fmt.Errorf("failed to create config directory: %w", err)
		}
	}

	// Seed config.yaml (always write to keep in sync with embedded defaults)
	if err := SeedFileFromFS(embedsFS, basePath, "config.yaml", filepath.Join(targetDir, "config.yaml"), force, true); err != nil {
		return fmt.Errorf("failed to seed config.yaml: %w", err)
	}

	// Seed home directory files from the harness embeds
	// Walk all files in the embed FS and place them under home/
	// except config.yaml and scion-agent.yaml which go at the top level
	err := fs.WalkDir(embedsFS, basePath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		// Get filename relative to the base path
		relPath, err := filepath.Rel(basePath, path)
		if err != nil {
			return err
		}

		// Skip config.yaml (already handled separately)
		if relPath == "config.yaml" {
			return nil
		}

		targetPath := mapEmbedFileToHarnessConfigPath(targetDir, homeDir, configDir, relPath)
		if targetPath == "" {
			return nil
		}

		// Ensure parent directory exists
		if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
			return err
		}

		return SeedFileFromFS(embedsFS, basePath, relPath, targetPath, force, false)
	})
	if err != nil {
		return fmt.Errorf("failed to seed harness-config files: %w", err)
	}

	return nil
}

func mapEmbedFileToHarnessConfigPath(targetDir, homeDir, configDir, fileName string) string {
	cleanName := filepath.ToSlash(filepath.Clean(fileName))
	if cleanName == "." || cleanName == ".." || cleanName == "" || cleanName == "config.yaml" || cleanName == "scion-agent.yaml" {
		return ""
	}
	if strings.HasPrefix(cleanName, "../") || filepath.IsAbs(cleanName) {
		return ""
	}

	if strings.HasPrefix(cleanName, "home/") {
		return filepath.Join(homeDir, filepath.FromSlash(strings.TrimPrefix(cleanName, "home/")))
	}

	if isHarnessConfigRootSupportFile(cleanName) {
		return filepath.Join(targetDir, filepath.FromSlash(cleanName))
	}

	return mapEmbedFileToHomePath(homeDir, configDir, cleanName)
}

func isHarnessConfigRootSupportFile(relPath string) bool {
	if relPath == "provision.py" || relPath == "dialect.yaml" {
		return true
	}
	for _, prefix := range []string{"schema/", "schemas/", "examples/", "tests/fixtures/"} {
		if strings.HasPrefix(relPath, prefix) {
			return true
		}
	}
	return false
}

// mapEmbedFileToHomePath maps an embed filename to its target path under homeDir.
// Files are placed according to harness conventions.
func mapEmbedFileToHomePath(homeDir, configDir, fileName string) string {
	switch fileName {
	case "bashrc":
		return filepath.Join(homeDir, ".bashrc")
	case "settings.json", "system_prompt.md":
		if configDir != "" {
			return filepath.Join(homeDir, configDir, fileName)
		}
		return ""
	case ".claude.json":
		return filepath.Join(homeDir, ".claude.json")
	case ".geminiignore":
		if configDir != "" {
			return filepath.Join(homeDir, configDir, ".geminiignore")
		}
		return filepath.Join(homeDir, ".geminiignore")
	case "config.toml":
		return filepath.Join(homeDir, ".codex", "config.toml")
	case "scion_notify.sh":
		return filepath.Join(homeDir, ".codex", "scion_notify.sh")
	case "opencode.json":
		if configDir != "" {
			return filepath.Join(homeDir, configDir, "opencode.json")
		}
		return ""
	default:
		// For unknown files, place them in the config dir if available
		if configDir != "" {
			return filepath.Join(homeDir, configDir, fileName)
		}
		return filepath.Join(homeDir, fileName)
	}
}

// ComputeHarnessConfigRevision returns a content hash uniquely identifying
// the on-disk state of a harness-config directory. The hash combines the
// SHA-256 of every file under dirPath (sorted by relative path) so two dirs
// with identical content produce the same revision string.
//
// Used by Phase 3 to stamp api.AgentInfo.HarnessConfigRevision when an
// agent is provisioned. For Hub-distributed harness-configs the value will
// match the manifest ContentHash recorded by the Hub's sync machinery; for
// local-only or built-in seeded configs it provides a stable local
// revision useful for audit.
//
// Returns "" when dirPath is empty or unreadable. Errors hashing individual
// files are skipped so a transient FS error does not block agent creation;
// the result still reflects the readable subset of files.
func ComputeHarnessConfigRevision(dirPath string) string {
	if dirPath == "" {
		return ""
	}
	files, err := os.ReadDir(dirPath)
	if err != nil || len(files) == 0 {
		return ""
	}
	type fileHash struct {
		Path string
		Hash string
	}
	var hashes []fileHash
	walk := func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(dirPath, path)
		if relErr != nil {
			return nil
		}
		f, openErr := os.Open(path)
		if openErr != nil {
			return nil
		}
		defer f.Close()
		h := sha256.New()
		if _, copyErr := io.Copy(h, f); copyErr != nil {
			return nil
		}
		hashes = append(hashes, fileHash{
			Path: filepath.ToSlash(rel),
			Hash: hex.EncodeToString(h.Sum(nil)),
		})
		return nil
	}
	if err := filepath.WalkDir(dirPath, walk); err != nil {
		return ""
	}
	if len(hashes) == 0 {
		return ""
	}
	sort.Slice(hashes, func(i, j int) bool { return hashes[i].Path < hashes[j].Path })
	combined := sha256.New()
	for _, fh := range hashes {
		combined.Write([]byte(fh.Path))
		combined.Write([]byte{0})
		combined.Write([]byte(fh.Hash))
		combined.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(combined.Sum(nil))
}

// SeedHarnessConfigFromFS is a lower-level function that seeds from a provided embed.FS.
// Used internally and for testing.
func SeedHarnessConfigFromFS(targetDir string, embedsFS embed.FS, basePath, configDir string, force bool) error {
	homeDir := filepath.Join(targetDir, "home")

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return fmt.Errorf("failed to create harness-config directory: %w", err)
	}
	if err := os.MkdirAll(homeDir, 0755); err != nil {
		return fmt.Errorf("failed to create harness-config home directory: %w", err)
	}

	if configDir != "" {
		if err := os.MkdirAll(filepath.Join(homeDir, configDir), 0755); err != nil {
			return fmt.Errorf("failed to create config directory: %w", err)
		}
	}

	// Seed config.yaml
	if err := SeedFileFromFS(embedsFS, basePath, "config.yaml", filepath.Join(targetDir, "config.yaml"), force, true); err != nil {
		return err
	}

	// Walk and seed home files
	return fs.WalkDir(embedsFS, basePath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		relPath, err := filepath.Rel(basePath, path)
		if err != nil {
			return err
		}

		switch relPath {
		case "config.yaml", "scion-agent.yaml":
			return nil
		}

		targetPath := mapEmbedFileToHarnessConfigPath(targetDir, homeDir, configDir, relPath)
		if targetPath == "" {
			return nil
		}

		if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
			return err
		}

		return SeedFileFromFS(embedsFS, basePath, relPath, targetPath, force, false)
	})
}
