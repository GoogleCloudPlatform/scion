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

package runtimebroker

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// preResolvedSkillsBody builds a minimal "preResolvedSkills" JSON payload
// (matching the Hub's /skills/resolve response shape) that resolves uri
// successfully. Used to prove a resolver was attached to the dispatch
// context without needing a real Hub connection or network access — the
// broker's PreResolvedSkillResolver path (#1784) requires neither.
func preResolvedSkillsBody(uri string) string {
	return `{
		"preResolvedSkills": {
			"resolved": [{
				"uri": "` + uri + `",
				"name": "test-skill",
				"resolvedVersion": "1.0.0",
				"contentHash": "sha256:abc",
				"files": []
			}]
		}
	}`
}

func TestStartAgent_AttachesSkillResolver_PreResolvedSkills(t *testing.T) {
	srv := newTestServer(t)
	mgr := srv.manager.(*mockManager)

	const uri = "skill://scion/global/test-skill@1.0.0"
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/start", strings.NewReader(preResolvedSkillsBody(uri)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}
	if mgr.lastStartCtx == nil {
		t.Fatal("expected Start to be called with a captured context")
	}

	resolver := agent.SkillResolverFromContext(mgr.lastStartCtx)
	if resolver == nil {
		t.Fatal("expected startAgent to attach a skill resolver to the dispatch context (#1960)")
	}

	result, err := resolver.Resolve(mgr.lastStartCtx, []api.SkillReference{{URI: uri}}, agent.ResolveOpts{})
	if err != nil {
		t.Fatalf("resolver.Resolve returned error: %v", err)
	}
	if len(result.Resolved) != 1 || result.Resolved[0].URI != uri {
		t.Errorf("expected the pre-resolved skill to resolve successfully, got: %+v", result)
	}
}

func TestRestartAgent_AttachesSkillResolver_PreResolvedSkills(t *testing.T) {
	srv := newTestServer(t)
	mgr := srv.manager.(*mockManager)

	const uri = "skill://scion/global/test-skill@1.0.0"
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/restart", strings.NewReader(preResolvedSkillsBody(uri)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}
	if mgr.lastStartCtx == nil {
		t.Fatal("expected Start (via restart) to be called with a captured context")
	}

	resolver := agent.SkillResolverFromContext(mgr.lastStartCtx)
	if resolver == nil {
		t.Fatal("expected restartAgent to attach a skill resolver to the dispatch context (#1960)")
	}

	result, err := resolver.Resolve(mgr.lastStartCtx, []api.SkillReference{{URI: uri}}, agent.ResolveOpts{})
	if err != nil {
		t.Fatalf("resolver.Resolve returned error: %v", err)
	}
	if len(result.Resolved) != 1 || result.Resolved[0].URI != uri {
		t.Errorf("expected the pre-resolved skill to resolve successfully, got: %+v", result)
	}
}

func TestCreateAgent_AttachesSkillResolver_PreResolvedSkills(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()

	const uri = "skill://scion/global/test-skill@1.0.0"
	body := `{
		"name": "provisioned-agent",
		"id": "agent-uuid-456",
		"slug": "provisioned-agent",
		"provisionOnly": true,
		"config": {"template": "claude"},
		"preResolvedSkills": {
			"resolved": [{
				"uri": "` + uri + `",
				"name": "test-skill",
				"resolvedVersion": "1.0.0",
				"contentHash": "sha256:abc",
				"files": []
			}]
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, w.Code, w.Body.String())
	}
	if mgr.lastProvisionCtx == nil {
		t.Fatal("expected Provision to be called with a captured context")
	}

	resolver := agent.SkillResolverFromContext(mgr.lastProvisionCtx)
	if resolver == nil {
		t.Fatal("expected createAgent to keep attaching a skill resolver to the dispatch context (unchanged by #1960)")
	}
	result, err := resolver.Resolve(mgr.lastProvisionCtx, []api.SkillReference{{URI: uri}}, agent.ResolveOpts{})
	if err != nil {
		t.Fatalf("resolver.Resolve returned error: %v", err)
	}
	if len(result.Resolved) != 1 || result.Resolved[0].URI != uri {
		t.Errorf("expected the pre-resolved skill to resolve successfully, got: %+v", result)
	}
}

// TestStartAgent_NoResolverAttachedWithoutHubOrPreResolved is the parity
// check for #1960: when the start request carries neither a Hub connection
// nor PreResolvedSkills — the same as before the fix, for a request that
// genuinely has nothing to attach — attachSkillResolver must leave ctx
// unchanged (no resolver), so that a template with only optional skills is
// still skipped rather than erroring, exactly like the create path.
func TestStartAgent_NoResolverAttachedWithoutHubOrPreResolved(t *testing.T) {
	srv := newTestServer(t)
	mgr := srv.manager.(*mockManager)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/start", nil)
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}
	if mgr.lastStartCtx == nil {
		t.Fatal("expected Start to be called with a captured context")
	}
	if resolver := agent.SkillResolverFromContext(mgr.lastStartCtx); resolver != nil {
		t.Errorf("expected no skill resolver on ctx when neither a Hub connection nor PreResolvedSkills is present, got %T", resolver)
	}
}
