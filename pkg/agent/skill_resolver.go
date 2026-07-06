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

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"crypto/sha256"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

const (
	defaultMaxFileSize = 10 * 1024 * 1024 // 10MB per file
	downloadTimeout    = 30 * time.Second
	stagingDirPrefix   = ".skill-staging-"
)

// SkillResolver resolves skill references to downloadable file sets.
type SkillResolver interface {
	// Resolve takes a batch of skill references and returns resolved skills.
	// Errors for individual skills are returned per-skill, not as a single error,
	// so optional skills can be skipped while required skills fail.
	Resolve(ctx context.Context, refs []api.SkillReference, opts ResolveOpts) (*ResolveResult, error)
}

// ResolveOpts provides context for scope-based resolution.
type ResolveOpts struct {
	ProjectID string
	UserID    string
}

// ResolveResult contains the batch resolution outcome.
type ResolveResult struct {
	Resolved []ResolvedSkill
	Errors   []ResolveError
}

// ResolveError represents a single skill that failed resolution.
type ResolveError struct {
	URI     string
	Code    string
	Message string
}

// ResolvedSkill is a skill that was successfully resolved to downloadable files.
type ResolvedSkill struct {
	Name               string
	URI                string
	As                 string
	Version            string
	Hash               string // Bundle content hash (sha256:...)
	Files              []ResolvedFile
	Deprecated         bool   `json:"-"`
	DeprecationMessage string `json:"-"`
	ReplacementURI     string `json:"-"`
}

// DestName returns the directory name to use when installing this skill.
func (rs *ResolvedSkill) DestName() (string, error) {
	name := rs.Name
	if rs.As != "" {
		name = rs.As
	}
	if err := api.ValidateSkillName(name); err != nil {
		return "", fmt.Errorf("invalid skill destination name %q: %w", name, err)
	}
	return name, nil
}

// ResolvedFile represents a single file within a resolved skill bundle.
type ResolvedFile struct {
	Path string
	URL  string
	Hash string
	Size int64
}

// --- Context injection ---

type skillResolverContextKey struct{}

// ContextWithSkillResolver returns a new context with the SkillResolver attached.
func ContextWithSkillResolver(ctx context.Context, r SkillResolver) context.Context {
	return context.WithValue(ctx, skillResolverContextKey{}, r)
}

// SkillResolverFromContext retrieves the SkillResolver from the context, or nil if not set.
func SkillResolverFromContext(ctx context.Context) SkillResolver {
	r, _ := ctx.Value(skillResolverContextKey{}).(SkillResolver)
	return r
}

// ResolverNamer is an optional interface a SkillResolver can implement
// to provide a name for the resolution record.
type ResolverNamer interface {
	ResolverName() string
}

// resolverName returns the name to record for a resolver. If the resolver
// implements ResolverNamer, its name is used; otherwise "unknown".
func resolverName(r SkillResolver) string {
	if n, ok := r.(ResolverNamer); ok {
		return n.ResolverName()
	}
	return "unknown"
}

type resolveProjectIDKey struct{}
type resolveUserIDKey struct{}

// ContextWithResolveProjectID returns a context carrying the project ID for skill resolution.
func ContextWithResolveProjectID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, resolveProjectIDKey{}, id)
}

// ResolveProjectIDFromContext retrieves the project ID for skill resolution.
func ResolveProjectIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(resolveProjectIDKey{}).(string)
	return v
}

// ContextWithResolveUserID returns a context carrying the user ID for skill resolution.
func ContextWithResolveUserID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, resolveUserIDKey{}, id)
}

// ResolveUserIDFromContext retrieves the user ID for skill resolution.
func ResolveUserIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(resolveUserIDKey{}).(string)
	return v
}

// --- Resolution record types ---

// SkillResolutionRecord is written to agentHome/.scion/resolved-skills.json
// after successful skill installation.
type SkillResolutionRecord struct {
	ResolvedAt string                 `json:"resolvedAt"`
	Resolver   string                 `json:"resolver"`
	Skills     []SkillResolutionEntry `json:"skills"`
}

// SkillResolutionEntry records a single installed skill.
type SkillResolutionEntry struct {
	URI                string      `json:"uri"`
	Name               string      `json:"name"`
	As                 string      `json:"as,omitempty"`
	ResolvedVersion    string      `json:"resolvedVersion"`
	ContentHash        string      `json:"contentHash"`
	Scope              string      `json:"scope"`
	InstalledPath      string      `json:"installedPath"`
	Source             string      `json:"source"`
	Files              []FileEntry `json:"files"`
	Deprecated         bool        `json:"deprecated,omitempty"`
	DeprecationMessage string      `json:"deprecationMessage,omitempty"`
	ReplacementURI     string      `json:"replacementUri,omitempty"`
}

// FileEntry records a single file within an installed skill.
type FileEntry struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
}

// --- Download, stage, verify, install ---

// installResolvedSkills downloads, verifies, and installs resolved skills
// into the agent's skill directory.
func installResolvedSkills(
	ctx context.Context,
	skills []ResolvedSkill,
	skillsDest string,
	agentHome string,
) (*SkillResolutionRecord, error) {
	// S6: Detect duplicate destinations
	destMap := make(map[string]string) // destName → URI
	for _, skill := range skills {
		dest, err := skill.DestName()
		if err != nil {
			return nil, err
		}
		if existing, ok := destMap[dest]; ok {
			return nil, fmt.Errorf(
				"skill resolution conflict: two skills resolve to the same destination directory %q:\n  - %s\n  - %s",
				dest, existing, skill.URI)
		}
		destMap[dest] = skill.URI
	}

	if err := os.MkdirAll(skillsDest, 0755); err != nil {
		return nil, fmt.Errorf("failed to create skills directory: %w", err)
	}

	record := &SkillResolutionRecord{
		ResolvedAt: time.Now().UTC().Format(time.RFC3339),
		Resolver:   "mock",
	}

	for _, skill := range skills {
		dest, _ := skill.DestName() // already validated above

		entry, err := installOneSkill(ctx, skill, dest, skillsDest)
		if err != nil {
			return nil, fmt.Errorf("skill %q installation failed: %w", skill.URI, err)
		}
		record.Skills = append(record.Skills, *entry)

		if skill.Deprecated {
			msg := fmt.Sprintf("Warning: skill %s@%s is deprecated", skill.Name, skill.Version)
			if skill.DeprecationMessage != "" {
				msg += ": " + skill.DeprecationMessage
			}
			if skill.ReplacementURI != "" {
				msg += fmt.Sprintf(" (replacement: %s)", skill.ReplacementURI)
			}
			fmt.Fprintln(os.Stderr, msg)
		}
	}

	return record, nil
}

func installOneSkill(ctx context.Context, skill ResolvedSkill, dest, skillsDest string) (*SkillResolutionEntry, error) {
	// Check cache before downloading
	cache := SkillCacheFromContext(ctx)
	if cache != nil && skill.Hash != "" {
		if cachedPath, hit := cache.Get(skill.Hash); hit {
			finalDest := filepath.Join(skillsDest, dest)
			if _, err := os.Stat(finalDest); err == nil {
				_ = os.RemoveAll(finalDest)
			}
			if err := cache.CopyToDir(cachedPath, finalDest); err == nil {
				if err := verifyInstalledSkillHash(finalDest, skill); err != nil {
					util.Debugf("provision: cached skill failed verification, falling through to download: %v", err)
					_ = os.RemoveAll(finalDest)
				} else {
					util.Debugf("provision: skill installed from cache: %s@%s", skill.Name, skill.Version)
					return buildSkillEntry(skill, dest, skillsDest)
				}
			}
			// Cache copy failed — fall through to download
		}
	}

	// Create staging directory
	stagingDir, err := os.MkdirTemp(skillsDest, stagingDirPrefix)
	if err != nil {
		return nil, fmt.Errorf("failed to create staging directory: %w", err)
	}
	defer func() {
		// Clean up staging dir on any failure
		_ = os.RemoveAll(stagingDir)
	}()

	skillStagingDir := filepath.Join(stagingDir, dest)
	if err := os.MkdirAll(skillStagingDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create skill staging dir: %w", err)
	}

	var fileEntries []FileEntry

	for _, f := range skill.Files {
		// S3: Validate path safety
		if err := validateFilePath(f.Path); err != nil {
			return nil, fmt.Errorf("unsafe file path in skill %q: %w", skill.URI, err)
		}

		destPath := filepath.Join(skillStagingDir, f.Path)

		// Create parent directories for nested files
		if dir := filepath.Dir(destPath); dir != skillStagingDir {
			if err := os.MkdirAll(dir, 0755); err != nil {
				return nil, fmt.Errorf("failed to create directory for %s: %w", f.Path, err)
			}
		}

		// S5: Download with transport constraints
		if err := downloadSkillFile(ctx, f.URL, destPath, defaultMaxFileSize); err != nil {
			return nil, fmt.Errorf("failed to download %s: %w", f.Path, err)
		}

		// S2: Verify per-file hash
		actualHash, err := transfer.HashFile(destPath)
		if err != nil {
			return nil, fmt.Errorf("failed to hash %s: %w", f.Path, err)
		}
		if actualHash != f.Hash {
			return nil, fmt.Errorf(
				"hash mismatch for file %q in skill %q: expected %s, got %s",
				f.Path, skill.URI, f.Hash, actualHash)
		}

		fileEntries = append(fileEntries, FileEntry{
			Path: f.Path,
			Hash: actualHash,
		})
	}

	// S2: Verify bundle hash
	if skill.Hash != "" {
		var transferFiles []transfer.FileInfo
		for _, fe := range fileEntries {
			transferFiles = append(transferFiles, transfer.FileInfo{
				Path: fe.Path,
				Hash: fe.Hash,
			})
		}
		bundleHash := transfer.ComputeContentHash(transferFiles)
		if bundleHash != skill.Hash {
			return nil, fmt.Errorf(
				"bundle hash mismatch for skill %q: expected %s, got %s",
				skill.URI, skill.Hash, bundleHash)
		}
	}

	// S3: Atomic install — remove existing destination and rename
	finalDest := filepath.Join(skillsDest, dest)
	if _, err := os.Stat(finalDest); err == nil {
		if err := os.RemoveAll(finalDest); err != nil {
			return nil, fmt.Errorf("failed to remove existing skill dir %s: %w", dest, err)
		}
	}
	if err := os.Rename(skillStagingDir, finalDest); err != nil {
		return nil, fmt.Errorf("failed to install skill %s: %w", dest, err)
	}

	// Populate cache after successful download+verify+install
	if cache != nil && skill.Hash != "" {
		populateSkillCache(cache, skill, finalDest)
	}

	return buildSkillEntry(skill, dest, skillsDest)
}

// buildSkillEntry creates a SkillResolutionEntry for a successfully installed skill.
func buildSkillEntry(skill ResolvedSkill, dest, skillsDest string) (*SkillResolutionEntry, error) {
	var scope string
	parsed, err := api.ParseSkillURI(skill.URI)
	if err == nil {
		scope = parsed.Scope
	}

	var fileEntries []FileEntry
	for _, f := range skill.Files {
		fileEntries = append(fileEntries, FileEntry{
			Path: f.Path,
			Hash: f.Hash,
		})
	}

	return &SkillResolutionEntry{
		URI:                skill.URI,
		Name:               skill.Name,
		As:                 skill.As,
		ResolvedVersion:    skill.Version,
		ContentHash:        skill.Hash,
		Scope:              scope,
		InstalledPath:      filepath.ToSlash(filepath.Join(filepath.Base(skillsDest), dest)),
		Source:             "registry",
		Files:              fileEntries,
		Deprecated:         skill.Deprecated,
		DeprecationMessage: skill.DeprecationMessage,
		ReplacementURI:     skill.ReplacementURI,
	}, nil
}

// populateSkillCache stores downloaded skill files in the cache.
func populateSkillCache(cache interface {
	Put(string, map[string][]byte) (string, error)
}, skill ResolvedSkill, installedDir string) {
	files := make(map[string][]byte, len(skill.Files))
	for _, f := range skill.Files {
		content, err := os.ReadFile(filepath.Join(installedDir, f.Path))
		if err != nil {
			util.Debugf("provision: failed to read skill file for caching: %s: %v", f.Path, err)
			return
		}
		files[f.Path] = content
	}
	if _, err := cache.Put(skill.Hash, files); err != nil {
		util.Debugf("provision: failed to cache skill %s@%s: %v", skill.Name, skill.Version, err)
	} else {
		util.Debugf("provision: cached skill %s@%s (%s)", skill.Name, skill.Version, skill.Hash)
	}
}

func verifyInstalledSkillHash(dir string, skill ResolvedSkill) error {
	for _, f := range skill.Files {
		path := filepath.Join(dir, f.Path)
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("missing file %s: %w", f.Path, err)
		}
		computed := fmt.Sprintf("sha256:%x", sha256.Sum256(data))
		if computed != f.Hash {
			return fmt.Errorf("hash mismatch for %s: expected %s, got %s", f.Path, f.Hash, computed)
		}
	}
	return nil
}

// validateFilePath checks that a relative path is safe for extraction.
func validateFilePath(path string) error {
	if path == "" {
		return fmt.Errorf("empty file path")
	}

	// Check for NUL bytes
	if strings.ContainsRune(path, 0) {
		return fmt.Errorf("path contains NUL byte")
	}

	// Check for backslashes
	if strings.Contains(path, "\\") {
		return fmt.Errorf("path contains backslash: %q", path)
	}

	// Clean the path and check for absolute paths
	cleaned := filepath.Clean(path)
	if filepath.IsAbs(cleaned) {
		return fmt.Errorf("absolute path not allowed: %q", path)
	}

	// Check for .. components
	for _, component := range strings.Split(cleaned, string(filepath.Separator)) {
		if component == ".." {
			return fmt.Errorf("path traversal not allowed: %q", path)
		}
	}

	// Check for OS-reserved names (Windows-safe, defensive)
	reserved := map[string]bool{
		"CON": true, "PRN": true, "AUX": true, "NUL": true,
		"COM1": true, "COM2": true, "COM3": true, "COM4": true,
		"COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
		"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true,
		"LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
	}
	baseName := strings.ToUpper(filepath.Base(cleaned))
	// Strip extension for reserved name check
	if idx := strings.IndexByte(baseName, '.'); idx >= 0 {
		baseName = baseName[:idx]
	}
	if reserved[baseName] {
		return fmt.Errorf("OS-reserved file name not allowed: %q", path)
	}

	return nil
}

// downloadSkillFile downloads a single file from a URL to a local path.
func downloadSkillFile(ctx context.Context, fileURL, destPath string, maxSize int64) error {
	parsed, err := url.Parse(fileURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	// S5: HTTPS only (except localhost)
	if parsed.Scheme != "https" {
		host := parsed.Hostname()
		if parsed.Scheme == "http" && isLocalhost(host) {
			// Allow HTTP for localhost
		} else {
			return fmt.Errorf("HTTPS required for skill downloads (got %s)", parsed.Scheme)
		}
	}

	client := &http.Client{
		Timeout: downloadTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// S5: No cross-host redirects
			if len(via) > 0 && req.URL.Host != via[0].URL.Host {
				return fmt.Errorf("cross-host redirect not allowed: %s → %s", via[0].URL.Host, req.URL.Host)
			}
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed with status %d", resp.StatusCode)
	}

	// S5: Enforce size limit
	limitedReader := io.LimitReader(resp.Body, maxSize+1)

	f, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}
	defer func() { _ = f.Close() }()

	n, err := io.Copy(f, limitedReader)
	if err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}
	if n > maxSize {
		_ = f.Close()
		_ = os.Remove(destPath)
		return fmt.Errorf("file exceeds maximum size of %d bytes", maxSize)
	}

	// S5: Do not log the URL (may contain signed tokens)
	util.Debugf("provision: downloaded skill file %s (%d bytes)", filepath.Base(destPath), n)

	return nil
}

func isLocalhost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// writeResolutionRecord writes the resolution record to disk.
func writeResolutionRecord(path string, record *SkillResolutionRecord) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// enumerateLocalSkills lists skills already present in the skills directory
// (from template's local skills/ dir) and returns them as resolution entries
// with source "local".
func enumerateLocalSkills(agentHome, skillsDir string) []SkillResolutionEntry {
	skillsPath := filepath.Join(agentHome, skillsDir)
	entries, err := os.ReadDir(skillsPath)
	if err != nil {
		return nil
	}

	var result []SkillResolutionEntry
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		entry := SkillResolutionEntry{
			Name:          e.Name(),
			InstalledPath: filepath.ToSlash(filepath.Join(skillsDir, e.Name())),
			Source:        "local",
		}
		result = append(result, entry)
	}
	return result
}

// collectRequiredSkillURIs returns URIs of non-optional skill references.
func collectRequiredSkillURIs(skills []api.SkillReference) []string {
	var uris []string
	for _, s := range skills {
		if !s.Optional {
			uris = append(uris, s.URI)
		}
	}
	return uris
}

// findRefByURI finds the first SkillReference matching the given URI.
func findRefByURI(refs []api.SkillReference, uri string) *api.SkillReference {
	for i := range refs {
		if refs[i].URI == uri {
			return &refs[i]
		}
	}
	return nil
}
