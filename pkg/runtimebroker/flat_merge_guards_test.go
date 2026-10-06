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
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
)

// Flat Runtime Broker guards on paths upstream added or reshaped
// (ptone/scion#3269): the harness-config policy reads settings through the
// flat view, and a flat instance's start context never classifies the saved
// or provisioned profile.

// makeFlat turns a test server into a flat instance (isFlat) without the
// rest of the flat hosting setup, which these unit-level checks do not use.
func makeFlat(srv *Server) {
	srv.config.FlatInstance = &FlatInstanceConfig{Identity: &brokeridentity.Identity{
		RuntimeBrokerID: "flat-broker-id",
		RuntimeTarget:   api.RuntimeTargetDescriptor{ID: "flat-target-id", Type: "docker"},
	}}
}

// TestFlatHarnessPolicy_IgnoresProfileDefaultHarnessConfig: with no explicit
// harness config, the policy evaluates the harness config the settings name.
// The active profile's default_harness_config names a container-script
// config the policy refuses; the settings' own default names a declarative
// one. A flat instance skips the profile tier, so it evaluates the
// declarative config and is not refused; a legacy server evaluates the
// profile's config and is refused (control).
func TestFlatHarnessPolicy_IgnoresProfileDefaultHarnessConfig(t *testing.T) {
	setup := func(t *testing.T) (*Server, string) {
		t.Helper()
		srv, _, _ := dispatchTestEnv(t, false) // container-script harnesses refused
		root := t.TempDir()
		dotScion := filepath.Join(root, ".scion")
		writeProjectSettings(t, dotScion, `schema_version: "1"
active_profile: p
default_harness_config: decl-hc
profiles:
  p:
    runtime: local-docker
    default_harness_config: scripted-hc
runtimes:
  local-docker:
    type: docker
`)
		writeHarnessConfig(t, dotScion, "scripted-hc", scriptedHarnessYAML)
		writeHarnessConfig(t, dotScion, "decl-hc", declarativeHarnessYAML)
		return srv, root
	}
	decide := func(srv *Server, root string) harnessPolicyDecision {
		return srv.enforceHarnessConfigPolicy(harnessPolicyInput{Req: CreateAgentRequest{Name: "policy-agent", ProjectPath: root}})
	}

	t.Run("legacy control", func(t *testing.T) {
		srv, root := setup(t)
		if d := decide(srv, root); d.OK {
			t.Fatal("control: a legacy server evaluates the active profile's default harness config (container-script) and must be refused")
		}
	})
	t.Run("flat", func(t *testing.T) {
		srv, root := setup(t)
		makeFlat(srv)
		if d := decide(srv, root); !d.OK {
			t.Fatalf("a flat instance must not take the profile's default harness config; refused with %s: %s", d.Code, d.Detail)
		}
	})
}

// TestFlatStartContext_NoProfileClassification: buildStartContext's
// preliminary runtime classification reads the agent's provisioned or saved
// profile (image-provenance.json) on start/restart and fails closed with 409
// on an unusable record. A flat instance does no profile-based
// classification there, so the same unusable record does not fail its start
// context; a legacy server gets the 409 (control).
func TestFlatStartContext_NoProfileClassification(t *testing.T) {
	in := func(projectDir, id string) startContextInputs {
		return startContextInputs{Name: id, AgentID: id, Slug: id, ProjectPath: projectDir, Operation: opHTTPRestart}
	}
	isClassificationConflict := func(err error) bool {
		var sce *startContextError
		return errors.As(err, &sce) && sce.Status == http.StatusConflict
	}

	t.Run("legacy control", func(t *testing.T) {
		srv, projectDir, id := runtimeProfileFixture(t, "{not json")
		_, err := srv.buildStartContext(context.Background(), in(projectDir, id))
		if !isClassificationConflict(err) {
			t.Fatalf("control: a legacy start context must fail closed with 409 on an unusable provenance record, got %v", err)
		}
	})
	t.Run("flat", func(t *testing.T) {
		srv, projectDir, id := runtimeProfileFixture(t, "{not json")
		makeFlat(srv)
		_, err := srv.buildStartContext(context.Background(), in(projectDir, id))
		if isClassificationConflict(err) {
			t.Fatalf("a flat instance must not classify the saved or provisioned profile in its start context, got %v", err)
		}
		if err != nil {
			t.Fatalf("flat start context: unexpected error %v", err)
		}
	})
}
