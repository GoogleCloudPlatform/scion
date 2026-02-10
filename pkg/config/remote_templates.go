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
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/sync"
	// Import rclone backends for remote template support
	// GCS and local are already imported in gcp/storage.go
	// Additional backends can be added as needed
	_ "github.com/rclone/rclone/backend/googlecloudstorage"
	_ "github.com/rclone/rclone/backend/local"
)

// RemoteTemplateType represents the type of remote template source
type RemoteTemplateType int

const (
	RemoteTypeUnknown RemoteTemplateType = iota
	RemoteTypeGitHub
	RemoteTypeArchive
	RemoteTypeRclone
)

// RemoteTemplate represents a template fetched from a remote source
type RemoteTemplate struct {
	URI       string
	Type      RemoteTemplateType
	CachePath string
}

// remoteCacheDir returns the directory where remote templates are cached.
func remoteCacheDir() (string, error) {
	globalDir, err := GetGlobalDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(globalDir, "cache", "remote-templates"), nil
}

// IsRemoteURI checks if the given string looks like a remote template URI.
// Returns true for:
// - URLs starting with http:// or https://
// - rclone connection strings starting with ":"
func IsRemoteURI(s string) bool {
	// rclone connection string (e.g., :gcs:bucket/path)
	if strings.HasPrefix(s, ":") {
		return true
	}

	// HTTP/HTTPS URLs
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return true
	}

	return false
}

// DetectRemoteType determines the type of remote template source.
func DetectRemoteType(uri string) RemoteTemplateType {
	// rclone connection string
	if strings.HasPrefix(uri, ":") {
		return RemoteTypeRclone
	}

	// Parse URL
	u, err := url.Parse(uri)
	if err != nil {
		return RemoteTypeUnknown
	}

	// GitHub folder URL
	if u.Host == "github.com" && !isArchiveURL(uri) {
		return RemoteTypeGitHub
	}

	// Archive URL
	if isArchiveURL(uri) {
		return RemoteTypeArchive
	}

	return RemoteTypeUnknown
}

// isArchiveURL checks if the URL points to a compressed archive.
func isArchiveURL(uri string) bool {
	lower := strings.ToLower(uri)
	return strings.HasSuffix(lower, ".tgz") ||
		strings.HasSuffix(lower, ".tar.gz") ||
		strings.HasSuffix(lower, ".zip")
}

// FetchRemoteTemplate fetches a template from a remote URI and caches it locally.
// Returns the local path to the cached template.
func FetchRemoteTemplate(ctx context.Context, uri string) (string, error) {
	cacheDir, err := remoteCacheDir()
	if err != nil {
		return "", fmt.Errorf("failed to get cache directory: %w", err)
	}

	// Create a unique cache key based on the URI
	cacheKey := generateCacheKey(uri)
	templateCachePath := filepath.Join(cacheDir, cacheKey)

	// For now, always fetch fresh (could add caching logic later)
	// Clean up any existing cache for this URI
	_ = os.RemoveAll(templateCachePath)

	if err := os.MkdirAll(templateCachePath, 0755); err != nil {
		return "", fmt.Errorf("failed to create cache directory: %w", err)
	}

	remoteType := DetectRemoteType(uri)

	switch remoteType {
	case RemoteTypeGitHub:
		if err := fetchGitHubFolder(ctx, uri, templateCachePath); err != nil {
			return "", fmt.Errorf("failed to fetch GitHub folder: %w", err)
		}
	case RemoteTypeArchive:
		if err := fetchArchive(ctx, uri, templateCachePath); err != nil {
			return "", fmt.Errorf("failed to fetch archive: %w", err)
		}
	case RemoteTypeRclone:
		if err := fetchRclone(ctx, uri, templateCachePath); err != nil {
			return "", fmt.Errorf("failed to fetch via rclone: %w", err)
		}
	default:
		return "", fmt.Errorf("unsupported remote template type: %s", uri)
	}

	return templateCachePath, nil
}

// generateCacheKey creates a unique, filesystem-safe key for a URI.
func generateCacheKey(uri string) string {
	hash := sha256.Sum256([]byte(uri))
	return fmt.Sprintf("%x", hash[:8]) // Use first 8 bytes (16 hex chars)
}

// fetchGitHubFolder fetches a folder from a GitHub repository.
// First attempts svn export, then falls back to sparse git checkout.
func fetchGitHubFolder(ctx context.Context, uri string, destPath string) error {
	// Parse GitHub URL to extract parts
	// Expected format: https://github.com/user/repo/tree/branch/path/to/folder
	// or: https://github.com/user/repo/blob/branch/path (for files, but we need folders)

	parsed, err := parseGitHubURL(uri)
	if err != nil {
		return err
	}

	// Try svn export first (cleaner, no .git files)
	if hasSvn() {
		svnURL := convertToSvnURL(parsed)
		if err := svnExport(ctx, svnURL, destPath); err == nil {
			return nil
		}
		// svn failed, fall through to git sparse checkout
	}

	// Fall back to sparse git checkout
	return sparseGitCheckout(ctx, parsed, destPath)
}

// GitHubURLParts contains parsed parts of a GitHub URL.
type GitHubURLParts struct {
	Owner  string
	Repo   string
	Branch string
	Path   string
}

// parseGitHubURL extracts components from a GitHub folder URL.
func parseGitHubURL(uri string) (*GitHubURLParts, error) {
	// Patterns to match:
	// https://github.com/owner/repo/tree/branch/path/to/folder
	// https://github.com/owner/repo/tree/branch (repo root at branch)

	u, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}

	if u.Host != "github.com" {
		return nil, fmt.Errorf("not a GitHub URL: %s", uri)
	}

	// Remove leading slash and split path
	pathParts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")

	if len(pathParts) < 2 {
		return nil, fmt.Errorf("invalid GitHub URL format, expected owner/repo: %s", uri)
	}

	result := &GitHubURLParts{
		Owner: pathParts[0],
		Repo:  pathParts[1],
	}

	// Check for tree/branch/path format
	if len(pathParts) >= 4 && pathParts[2] == "tree" {
		result.Branch = pathParts[3]
		if len(pathParts) > 4 {
			result.Path = strings.Join(pathParts[4:], "/")
		}
	} else if len(pathParts) == 2 {
		// Just owner/repo, default to main branch
		result.Branch = "main"
	} else {
		return nil, fmt.Errorf("unsupported GitHub URL format: %s", uri)
	}

	return result, nil
}

// hasSvn checks if svn is available in PATH.
func hasSvn() bool {
	_, err := exec.LookPath("svn")
	return err == nil
}

// convertToSvnURL converts a GitHub URL to an svn-compatible URL.
func convertToSvnURL(parts *GitHubURLParts) string {
	// SVN uses trunk for main/master, branches/X for other branches
	var svnPath string
	if parts.Branch == "main" || parts.Branch == "master" {
		svnPath = "trunk"
	} else {
		svnPath = "branches/" + parts.Branch
	}

	if parts.Path != "" {
		svnPath = svnPath + "/" + parts.Path
	}

	return fmt.Sprintf("https://github.com/%s/%s/%s", parts.Owner, parts.Repo, svnPath)
}

// svnExport exports a folder from GitHub using svn.
func svnExport(ctx context.Context, svnURL string, destPath string) error {
	// svn export downloads the folder contents directly into destPath
	cmd := exec.CommandContext(ctx, "svn", "export", "--force", svnURL, destPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// sparseGitCheckout performs a sparse git checkout to get only the needed folder.
func sparseGitCheckout(ctx context.Context, parts *GitHubURLParts, destPath string) error {
	// Create a temporary directory for the git clone
	tmpDir, err := os.MkdirTemp("", "scion-git-sparse-*")
	if err != nil {
		return fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	repoURL := fmt.Sprintf("https://github.com/%s/%s.git", parts.Owner, parts.Repo)

	// Initialize git repo with sparse checkout
	commands := [][]string{
		{"git", "init"},
		{"git", "remote", "add", "origin", repoURL},
		{"git", "config", "core.sparseCheckout", "true"},
	}

	for _, args := range commands {
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.Dir = tmpDir
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git command %v failed: %w", args, err)
		}
	}

	// Write sparse-checkout config
	sparseCheckoutPath := filepath.Join(tmpDir, ".git", "info", "sparse-checkout")
	if err := os.MkdirAll(filepath.Dir(sparseCheckoutPath), 0755); err != nil {
		return err
	}

	// If there's a path, only check out that path; otherwise, check out everything
	sparsePattern := "/*"
	if parts.Path != "" {
		sparsePattern = parts.Path + "/*"
	}
	if err := os.WriteFile(sparseCheckoutPath, []byte(sparsePattern+"\n"), 0644); err != nil {
		return fmt.Errorf("failed to write sparse-checkout config: %w", err)
	}

	// Fetch and checkout the branch
	fetchCmd := exec.CommandContext(ctx, "git", "fetch", "--depth=1", "origin", parts.Branch)
	fetchCmd.Dir = tmpDir
	if err := fetchCmd.Run(); err != nil {
		return fmt.Errorf("git fetch failed: %w", err)
	}

	checkoutCmd := exec.CommandContext(ctx, "git", "checkout", parts.Branch)
	checkoutCmd.Dir = tmpDir
	if err := checkoutCmd.Run(); err != nil {
		return fmt.Errorf("git checkout failed: %w", err)
	}

	// Copy the desired folder to the destination
	var srcPath string
	if parts.Path != "" {
		srcPath = filepath.Join(tmpDir, parts.Path)
	} else {
		srcPath = tmpDir
	}

	// Check if source exists
	if _, err := os.Stat(srcPath); os.IsNotExist(err) {
		return fmt.Errorf("path %s not found in repository", parts.Path)
	}

	// Copy contents to destination, excluding .git
	return copyDirExcludingGit(srcPath, destPath)
}

// copyDirExcludingGit copies a directory recursively, excluding .git directories.
func copyDirExcludingGit(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip .git directories
		if info.IsDir() && info.Name() == ".git" {
			return filepath.SkipDir
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}

		target := filepath.Join(dst, rel)

		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		return os.WriteFile(target, data, info.Mode())
	})
}

// fetchArchive downloads and extracts a compressed archive.
func fetchArchive(ctx context.Context, uri string, destPath string) error {
	// Download the archive to a temp file
	tmpFile, err := os.CreateTemp("", "scion-archive-*")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)
	defer tmpFile.Close()

	// Download
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to download archive: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to download archive: HTTP %d", resp.StatusCode)
	}

	if _, err := io.Copy(tmpFile, resp.Body); err != nil {
		return fmt.Errorf("failed to save archive: %w", err)
	}
	tmpFile.Close()

	// Extract based on file type
	lower := strings.ToLower(uri)
	if strings.HasSuffix(lower, ".zip") {
		return extractZip(tmpPath, destPath)
	} else if strings.HasSuffix(lower, ".tgz") || strings.HasSuffix(lower, ".tar.gz") {
		return extractTarGz(tmpPath, destPath)
	}

	return fmt.Errorf("unsupported archive format: %s", uri)
}

// extractZip extracts a zip archive to the destination path.
func extractZip(zipPath, destPath string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("failed to open zip: %w", err)
	}
	defer r.Close()

	// Detect if there's a common root directory
	commonRoot := detectCommonRoot(func(yield func(string) bool) {
		for _, f := range r.File {
			if !yield(f.Name) {
				return
			}
		}
	})

	for _, f := range r.File {
		name := f.Name

		// Strip common root if present
		if commonRoot != "" {
			name = strings.TrimPrefix(name, commonRoot)
			if name == "" {
				continue // Skip the root directory itself
			}
		}

		// Sanitize path to prevent zip slip attacks
		name = sanitizePath(name)
		if name == "" {
			continue
		}

		fpath := filepath.Join(destPath, name)

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(fpath, f.Mode()); err != nil {
				return err
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(fpath), 0755); err != nil {
			return err
		}

		outFile, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			return err
		}

		rc, err := f.Open()
		if err != nil {
			outFile.Close()
			return err
		}

		_, err = io.Copy(outFile, rc)
		rc.Close()
		outFile.Close()

		if err != nil {
			return err
		}
	}

	return nil
}

// extractTarGz extracts a .tar.gz or .tgz archive to the destination path.
func extractTarGz(tarGzPath, destPath string) error {
	f, err := os.Open(tarGzPath)
	if err != nil {
		return fmt.Errorf("failed to open archive: %w", err)
	}
	defer f.Close()

	gzr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gzr.Close()

	// First pass: detect common root
	tarReader := tar.NewReader(gzr)
	var entries []string
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read tar header: %w", err)
		}
		entries = append(entries, header.Name)
	}

	commonRoot := detectCommonRoot(func(yield func(string) bool) {
		for _, e := range entries {
			if !yield(e) {
				return
			}
		}
	})

	// Reopen for extraction
	f.Close()
	f, err = os.Open(tarGzPath)
	if err != nil {
		return fmt.Errorf("failed to reopen archive: %w", err)
	}
	defer f.Close()

	gzr, err = gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer gzr.Close()

	tarReader = tar.NewReader(gzr)

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read tar header: %w", err)
		}

		name := header.Name

		// Strip common root if present
		if commonRoot != "" {
			name = strings.TrimPrefix(name, commonRoot)
			if name == "" {
				continue
			}
		}

		// Sanitize path
		name = sanitizePath(name)
		if name == "" {
			continue
		}

		fpath := filepath.Join(destPath, name)

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(fpath, os.FileMode(header.Mode)); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(fpath), 0755); err != nil {
				return err
			}

			outFile, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(header.Mode))
			if err != nil {
				return err
			}

			if _, err := io.Copy(outFile, tarReader); err != nil {
				outFile.Close()
				return err
			}
			outFile.Close()
		}
	}

	return nil
}

// detectCommonRoot detects if all entries share a common root directory.
// This is common in archives created from a folder (e.g., repo-main/).
func detectCommonRoot(entries func(func(string) bool)) string {
	var firstDir string
	allShareRoot := true

	entries(func(name string) bool {
		// Get the first path component
		parts := strings.SplitN(name, "/", 2)
		if len(parts) == 0 {
			return true
		}

		dir := parts[0]
		if firstDir == "" {
			firstDir = dir + "/"
		} else if dir+"/" != firstDir && !strings.HasPrefix(name, firstDir) {
			allShareRoot = false
			return false // Stop iteration
		}
		return true
	})

	if allShareRoot && firstDir != "" {
		return firstDir
	}
	return ""
}

// sanitizePath prevents path traversal attacks.
func sanitizePath(name string) string {
	// Clean the path
	name = filepath.Clean(name)

	// Reject absolute paths
	if filepath.IsAbs(name) {
		return ""
	}

	// Reject paths that try to escape
	if strings.HasPrefix(name, "..") || strings.Contains(name, string(filepath.Separator)+"..") {
		return ""
	}

	return name
}

// fetchRclone fetches a template using rclone from a remote storage location.
func fetchRclone(ctx context.Context, uri string, destPath string) error {
	// uri is expected to be in rclone format, e.g., :gcs:bucket/path or :s3:bucket/path
	srcFs, err := fs.NewFs(ctx, uri)
	if err != nil {
		return fmt.Errorf("failed to create source fs for %s: %w", uri, err)
	}

	dstFs, err := fs.NewFs(ctx, destPath)
	if err != nil {
		return fmt.Errorf("failed to create destination fs for %s: %w", destPath, err)
	}

	// Sync from remote to local
	if err := sync.Sync(ctx, dstFs, srcFs, false); err != nil {
		return fmt.Errorf("rclone sync failed: %w", err)
	}

	return nil
}

// CleanRemoteTemplateCache cleans up the remote template cache.
func CleanRemoteTemplateCache() error {
	cacheDir, err := remoteCacheDir()
	if err != nil {
		return err
	}
	return os.RemoveAll(cacheDir)
}

// TODO: Future enhancement - simple template names may resolve to remote storage when
// operating with a remote hub system. The resolution could follow a pattern like:
// <bucket-name>/<scion-prefix>/<grove-id>/templates/<template-name>
// This would allow templates to be shared across a team or organization via
// a central hub, using both the current grove's location as well as a global
// location on the hub. The hub integration would need to provide:
// - Grove ID resolution
// - Hub bucket/prefix configuration
// - Fallback chain: local grove -> hub grove -> hub global -> error

// ValidateRemoteURI performs basic validation on a remote template URI.
func ValidateRemoteURI(uri string) error {
	if !IsRemoteURI(uri) {
		return fmt.Errorf("not a remote URI: %s", uri)
	}

	remoteType := DetectRemoteType(uri)

	switch remoteType {
	case RemoteTypeGitHub:
		// Validate GitHub URL format
		_, err := parseGitHubURL(uri)
		return err

	case RemoteTypeArchive:
		// Validate URL format
		_, err := url.Parse(uri)
		return err

	case RemoteTypeRclone:
		// Basic validation - should match :backend:path format
		if !regexp.MustCompile(`^:\w+:.+`).MatchString(uri) {
			return fmt.Errorf("invalid rclone URI format: %s (expected :backend:path)", uri)
		}
		return nil

	default:
		return fmt.Errorf("unsupported remote URI type: %s", uri)
	}
}
