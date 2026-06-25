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

package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	federationTimeout     = 30 * time.Second
	federationMaxBodySize = 10 * 1024 * 1024 // 10MB
)

// federateResolve proxies a skill resolve request to an external registry.
func (s *Server) federateResolve(ctx context.Context, registryName string, skillRef ResolveSkillRef) (*ResolvedSkillResponse, *ResolveSkillError) {
	registry, err := s.store.GetSkillRegistryByName(ctx, registryName)
	if err != nil {
		return nil, &ResolveSkillError{
			URI: skillRef.URI, Code: "unknown_registry",
			Message: fmt.Sprintf("registry %q is not configured", registryName),
		}
	}
	if registry.Status != store.SkillRegistryStatusActive {
		return nil, &ResolveSkillError{
			URI: skillRef.URI, Code: "registry_disabled",
			Message: fmt.Sprintf("registry %q is disabled", registryName),
		}
	}
	if registry.Type != store.SkillRegistryTypeHub {
		return nil, &ResolveSkillError{
			URI: skillRef.URI, Code: "wrong_registry_type",
			Message: fmt.Sprintf("registry %q is type %q, not hub", registryName, registry.Type),
		}
	}

	resolvePath := registry.ResolvePath
	if resolvePath == "" {
		resolvePath = "/api/v1/skills/resolve"
	}
	if !strings.HasPrefix(resolvePath, "/") {
		resolvePath = "/" + resolvePath
	}
	resolveURL := strings.TrimRight(registry.Endpoint, "/") + resolvePath

	remoteURI := skillRef.URI
	if prefix := "skill://" + registryName + "/"; strings.HasPrefix(remoteURI, prefix) {
		remoteURI = "skill:///" + strings.TrimPrefix(remoteURI, prefix)
	}
	proxyReq := &ResolveSkillsRequest{
		Skills: []ResolveSkillRef{{URI: remoteURI}},
	}
	body, err := json.Marshal(proxyReq)
	if err != nil {
		return nil, &ResolveSkillError{URI: skillRef.URI, Code: "internal_error", Message: err.Error()}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, resolveURL, bytes.NewReader(body))
	if err != nil {
		return nil, &ResolveSkillError{URI: skillRef.URI, Code: "internal_error", Message: err.Error()}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if registry.AuthToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+registry.AuthToken)
	}

	resp, err := s.federationClient.Do(httpReq)
	if err != nil {
		return nil, &ResolveSkillError{
			URI: skillRef.URI, Code: "federation_error",
			Message: fmt.Sprintf("failed to connect to registry %q: %v", registryName, err),
		}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, &ResolveSkillError{
			URI: skillRef.URI, Code: "federation_error",
			Message: fmt.Sprintf("registry %q returned %d: %s", registryName, resp.StatusCode, string(respBody)),
		}
	}

	var resolveResp ResolveSkillsResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, federationMaxBodySize)).Decode(&resolveResp); err != nil {
		return nil, &ResolveSkillError{
			URI: skillRef.URI, Code: "federation_error",
			Message: fmt.Sprintf("failed to decode response from registry %q: %v", registryName, err),
		}
	}

	if len(resolveResp.Errors) > 0 {
		return nil, &ResolveSkillError{
			URI:     skillRef.URI,
			Code:    resolveResp.Errors[0].Code,
			Message: fmt.Sprintf("registry %q: %s", registryName, resolveResp.Errors[0].Message),
		}
	}
	if len(resolveResp.Resolved) == 0 {
		return nil, &ResolveSkillError{
			URI: skillRef.URI, Code: "not_found",
			Message: fmt.Sprintf("skill not found in registry %q", registryName),
		}
	}

	resolved := &resolveResp.Resolved[0]

	// Trust enforcement
	if registry.TrustLevel == store.SkillRegistryTrustPinned {
		pinnedHash, err := s.store.GetPinnedHash(ctx, registry.ID, skillRef.URI)
		if err != nil {
			return nil, &ResolveSkillError{
				URI: skillRef.URI, Code: "trust_violation",
				Message: fmt.Sprintf("no pinned hash for %q in registry %q; use 'scion skills registries pin' first", skillRef.URI, registryName),
			}
		}
		if resolved.ContentHash != pinnedHash {
			return nil, &ResolveSkillError{
				URI: skillRef.URI, Code: "trust_violation",
				Message: fmt.Sprintf("content hash mismatch for %q from registry %q: expected %s, got %s",
					skillRef.URI, registryName, pinnedHash, resolved.ContentHash),
			}
		}
	}

	return resolved, nil
}
