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
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	federationTimeout     = 30 * time.Second
	federationMaxBodySize = 10 * 1024 * 1024 // 10MB
	// federationMaxErrorText bounds the text kept from a registry's error
	// response: a non-OK body, or the message of a per-skill error.
	federationMaxErrorText = 1024
)

// federationTokenPattern matches a code or correlation id from a registry
// that is used as it is: at most 64 characters of [A-Za-z0-9._-].
var federationTokenPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// federationInvalidToken is logged in place of a registry code or
// correlation id that federationTokenPattern does not match.
const federationInvalidToken = "(invalid)"

// boundedFederationToken returns s, a code or correlation id from a
// registry, for the log: s when it matches federationTokenPattern or is
// empty, else federationInvalidToken.
func boundedFederationToken(s string) string {
	if s == "" || federationTokenPattern.MatchString(s) {
		return s
	}
	return federationInvalidToken
}

// federationStatus returns status, an HTTP status from a registry (of
// its response or of a per-skill error), when it is in 100..599, else 0.
func federationStatus(status int) int {
	if status >= 100 && status <= 599 {
		return status
	}
	return 0
}

// truncateUTF8 returns s cut to at most limit bytes, backing off to the
// start of a UTF-8 sequence so that a character is not split.
func truncateUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// federateResolve proxies a skill resolve request to an external registry.
func (s *Server) federateResolve(ctx context.Context, registryName string, skillRef ResolveSkillRef) (*ResolvedSkillResponse, *ResolveSkillError) {
	registry, err := s.store.GetSkillRegistryByName(ctx, registryName)
	if err != nil {
		return nil, newSkillResolveError(ctx, skillResolveSourceFederation, skillRef.URI,
			agent.SkillErrCodeUnknownRegistry, 0, fmt.Sprintf("registry %q is not configured: %v", registryName, err))
	}
	if registry.Status != store.SkillRegistryStatusActive {
		return nil, newSkillResolveError(ctx, skillResolveSourceFederation, skillRef.URI,
			agent.SkillErrCodeRegistryDisabled, 0, fmt.Sprintf("registry %q is disabled", registryName))
	}
	if registry.Type != store.SkillRegistryTypeHub {
		return nil, newSkillResolveError(ctx, skillResolveSourceFederation, skillRef.URI,
			agent.SkillErrCodeWrongRegistryType, 0, fmt.Sprintf("registry %q is type %q, not hub", registryName, registry.Type))
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
		return nil, newSkillResolveError(ctx, skillResolveSourceFederation, skillRef.URI,
			agent.SkillErrCodeInternalError, 0, err.Error())
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, resolveURL, bytes.NewReader(body))
	if err != nil {
		return nil, newSkillResolveError(ctx, skillResolveSourceFederation, skillRef.URI,
			agent.SkillErrCodeInternalError, 0, err.Error())
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if registry.AuthToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+registry.AuthToken)
	}

	resp, err := s.federationClient.Do(httpReq)
	if err != nil {
		return nil, newSkillResolveError(ctx, skillResolveSourceFederation, skillRef.URI,
			agent.SkillErrCodeFederationError, 0, fmt.Sprintf("failed to connect to registry %q: %v", registryName, err))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, federationMaxErrorText))
		return nil, newSkillResolveError(ctx, skillResolveSourceFederation, skillRef.URI,
			agent.SkillErrCodeFederationError, federationStatus(resp.StatusCode),
			fmt.Sprintf("registry %q returned %d: %s", registryName, resp.StatusCode, string(respBody)),
			"remote_status", resp.StatusCode)
	}

	var resolveResp ResolveSkillsResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, federationMaxBodySize)).Decode(&resolveResp); err != nil {
		return nil, newSkillResolveError(ctx, skillResolveSourceFederation, skillRef.URI,
			agent.SkillErrCodeFederationError, 0, fmt.Sprintf("failed to decode response from registry %q: %v", registryName, err))
	}

	if len(resolveResp.Errors) > 0 {
		// The registry's code is returned when it is a bounded token, else
		// federation_error; its message, code and correlation id are logged
		// bounded, and its status is kept only when it is an HTTP status
		// (the log keeps the raw status as remote_status).
		remote := resolveResp.Errors[0]
		code := agent.SkillErrCodeFederationError
		if federationTokenPattern.MatchString(remote.Code) {
			code = remote.Code
		}
		return nil, newSkillResolveError(ctx, skillResolveSourceFederation, skillRef.URI,
			code, federationStatus(remote.Status),
			fmt.Sprintf("registry %q: %s", registryName, truncateUTF8(remote.Message, federationMaxErrorText)),
			"remote_status", remote.Status,
			"remote_code", boundedFederationToken(remote.Code),
			"remote_correlation_id", boundedFederationToken(remote.CorrelationID))
	}
	if len(resolveResp.Resolved) == 0 {
		return nil, newSkillResolveError(ctx, skillResolveSourceFederation, skillRef.URI,
			agent.SkillErrCodeNotFound, 0, fmt.Sprintf("skill not found in registry %q", registryName))
	}

	resolved := &resolveResp.Resolved[0]

	// Content hash checks and trust enforcement (ptone/scion#5935).
	if resolveErr := s.checkFederatedSkillHashes(ctx, registry, registryName, skillRef, resolved); resolveErr != nil {
		return nil, resolveErr
	}

	return resolved, nil
}
