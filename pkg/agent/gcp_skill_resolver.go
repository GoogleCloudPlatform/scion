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
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const (
	gcpAPITimeout = 30 * time.Second
	gcpScope      = "https://www.googleapis.com/auth/cloud-platform"
)

// RegistryLookupResult holds the registry configuration needed by the GCP resolver.
type RegistryLookupResult struct {
	Name     string
	Endpoint string
	Type     string
	Status   string
}

// RegistryLookup resolves a registry alias to its configuration.
type RegistryLookup func(ctx context.Context, name string) (*RegistryLookupResult, error)

// GCPSkillResolver resolves skills from GCP Vertex AI skill registries.
type GCPSkillResolver struct {
	registryLookup RegistryLookup
	httpClient     *http.Client
	tokenSource    func(ctx context.Context) (string, error)
	tokenOnce      sync.Once
	cachedTS       oauth2.TokenSource
	tokenErr       error
}

// NewGCPSkillResolver creates a resolver for gcp-skill:// URIs.
func NewGCPSkillResolver(lookup RegistryLookup) *GCPSkillResolver {
	return &GCPSkillResolver{
		registryLookup: lookup,
		httpClient: &http.Client{
			Timeout: gcpAPITimeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (r *GCPSkillResolver) ResolverName() string { return "gcp" }

func (r *GCPSkillResolver) Resolve(ctx context.Context, refs []api.SkillReference, opts ResolveOpts) (*ResolveResult, error) {
	result := &ResolveResult{}

	for _, ref := range refs {
		gcpRef, err := ParseGCPSkillURI(ref.URI)
		if err != nil {
			result.Errors = append(result.Errors, ResolveError{
				URI: ref.URI, Code: "invalid_uri", Message: err.Error(),
			})
			continue
		}

		resolved, err := r.resolveOne(ctx, gcpRef, ref)
		if err != nil {
			result.Errors = append(result.Errors, ResolveError{
				URI: ref.URI, Code: "resolve_failed", Message: err.Error(),
			})
			continue
		}
		result.Resolved = append(result.Resolved, *resolved)
	}

	return result, nil
}

func (r *GCPSkillResolver) resolveOne(ctx context.Context, gcpRef *GCPSkillRef, ref api.SkillReference) (*ResolvedSkill, error) {
	registry, err := r.registryLookup(ctx, gcpRef.Alias)
	if err != nil {
		return nil, fmt.Errorf("registry alias %q not found: %w", gcpRef.Alias, err)
	}
	if registry == nil {
		return nil, fmt.Errorf("registry alias %q lookup returned nil", gcpRef.Alias)
	}
	if registry.Status != "active" {
		return nil, fmt.Errorf("registry %q is disabled", gcpRef.Alias)
	}
	if registry.Type != "gcp" {
		return nil, fmt.Errorf("registry %q is type %q, expected gcp", gcpRef.Alias, registry.Type)
	}

	resourceURL, err := url.JoinPath(registry.Endpoint, gcpRef.SkillID)
	if err != nil {
		return nil, fmt.Errorf("invalid registry endpoint URL: %w", err)
	}

	token, err := r.getADCToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("GCP authentication failed: %w", err)
	}

	skill, err := r.fetchSkillMetadata(ctx, resourceURL, token)
	if err != nil {
		return nil, err
	}

	if gcpRef.Version != "" && skill.Version != gcpRef.Version {
		return nil, fmt.Errorf("requested version %q but GCP API returned %q", gcpRef.Version, skill.Version)
	}

	registryHost, err := urlHost(registry.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid registry endpoint: %w", err)
	}

	var resolvedFiles []ResolvedFile
	var fileInfos []transfer.FileInfo

	for _, f := range skill.Files {
		if err := validateFileURL(f.URL, registryHost); err != nil {
			return nil, fmt.Errorf("unsafe file URL for %s: %w", f.Path, err)
		}

		content, err := r.downloadFile(ctx, f.URL, token)
		if err != nil {
			return nil, fmt.Errorf("failed to download %s: %w", f.Path, err)
		}

		hash := fmt.Sprintf("sha256:%x", sha256.Sum256(content))
		resolvedFiles = append(resolvedFiles, ResolvedFile{
			Path: f.Path,
			URL:  f.URL,
			Hash: hash,
			Size: int64(len(content)),
		})
		fileInfos = append(fileInfos, transfer.FileInfo{Path: f.Path, Hash: hash})
	}

	if len(resolvedFiles) == 0 {
		return nil, fmt.Errorf("GCP skill %q has no files", gcpRef.SkillID)
	}

	bundleHash := transfer.ComputeContentHash(fileInfos)

	return &ResolvedSkill{
		Name:    gcpRef.SkillID,
		URI:     gcpRef.Raw,
		As:      ref.As,
		Version: skill.Version,
		Hash:    bundleHash,
		Files:   resolvedFiles,
	}, nil
}

func (r *GCPSkillResolver) getADCToken(ctx context.Context) (string, error) {
	if r.tokenSource != nil {
		return r.tokenSource(ctx)
	}

	r.tokenOnce.Do(func() {
		creds, err := google.FindDefaultCredentials(context.Background(), gcpScope)
		if err != nil {
			r.tokenErr = fmt.Errorf("no GCP credentials found (set GOOGLE_APPLICATION_CREDENTIALS or use 'gcloud auth application-default login'): %w", err)
			return
		}
		r.cachedTS = creds.TokenSource
	})
	if r.tokenErr != nil {
		return "", r.tokenErr
	}

	tok, err := r.cachedTS.Token()
	if err != nil {
		return "", fmt.Errorf("failed to obtain GCP token: %w", err)
	}

	return tok.AccessToken, nil
}

// gcpSkillResponse represents the GCP Vertex AI skill metadata response.
type gcpSkillResponse struct {
	Name        string         `json:"name"`
	DisplayName string         `json:"displayName"`
	Version     string         `json:"version"`
	Files       []gcpSkillFile `json:"files"`
}

type gcpSkillFile struct {
	Path string `json:"path"`
	URL  string `json:"url"`
}

func (r *GCPSkillResolver) fetchSkillMetadata(ctx context.Context, resourceURL, token string) (*gcpSkillResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, resourceURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GCP API request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("skill not found in GCP registry")
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("GCP API access denied — check service account permissions")
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("GCP API error (%d): %s", resp.StatusCode, string(body))
	}

	const maxMetadataSize = 1 * 1024 * 1024 // 1MB
	var skill gcpSkillResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxMetadataSize)).Decode(&skill); err != nil {
		return nil, fmt.Errorf("failed to decode GCP API response: %w", err)
	}

	return &skill, nil
}

func (r *GCPSkillResolver) downloadFile(ctx context.Context, fileURL, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download failed with status %d", resp.StatusCode)
	}

	content, err := io.ReadAll(io.LimitReader(resp.Body, int64(defaultMaxFileSize)+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}
	if int64(len(content)) > int64(defaultMaxFileSize) {
		return nil, fmt.Errorf("file exceeds maximum size of %d bytes", defaultMaxFileSize)
	}
	return content, nil
}

func urlHost(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	return u.Hostname(), nil
}

// validateFileURL checks that a file download URL is safe to fetch:
// it must use HTTPS, share the same host as the registry endpoint,
// and not target internal/link-local addresses.
func validateFileURL(fileURL, registryHost string) error {
	u, err := url.Parse(fileURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	host := u.Hostname()

	if u.Scheme != "https" && !isLocalhost(host) {
		return fmt.Errorf("HTTPS required for file downloads (got %s)", u.Scheme)
	}

	if isBlockedHost(host) && !isLocalhost(host) {
		return fmt.Errorf("file URL targets blocked address: %s", host)
	}

	if !strings.EqualFold(host, registryHost) {
		return fmt.Errorf("file URL host %q does not match registry host %q", host, registryHost)
	}

	return nil
}

func isBlockedHost(host string) bool {
	blocked := []string{"metadata.google.internal", "metadata.google.internal."}
	for _, b := range blocked {
		if strings.EqualFold(host, b) {
			return true
		}
	}

	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}

	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsPrivate()
}
