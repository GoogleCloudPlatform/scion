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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// templateIdentityFixture is a project directory and the start options a
// start or restart of agentName in it builds, for the
// applyStartTemplateIdentity tests (ptone/scion#2157).
type templateIdentityFixture struct {
	srv        *Server
	projectDir string
	opts       api.StartOptions
}

func newTemplateIdentityFixture(t *testing.T, agentName string, provisioned bool) *templateIdentityFixture {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	projectDir := filepath.Join(t.TempDir(), ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	if provisioned {
		agentDir := config.GetAgentDir(projectDir, agentName, false)
		require.NoError(t, os.MkdirAll(filepath.Join(agentDir, "home"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{"harness":"claude"}`), 0o644))
	}
	return &templateIdentityFixture{
		srv:        newTestServer(t),
		projectDir: projectDir,
		opts:       api.StartOptions{Name: agentName, ProjectPath: projectDir, BrokerMode: true},
	}
}

func (f *templateIdentityFixture) apply(in startContextInputs, conn *HubConnection, slug string) error {
	return f.srv.applyStartTemplateIdentity(context.Background(), in, &f.opts, conn, slug)
}

// templateConn is a co-located Hub connection whose local storage holds the
// global template slug; getErr, when set, fails the template lookup.
func templateConn(t *testing.T, slug string, getErr error, onGet func()) (*HubConnection, string) {
	t.Helper()
	stor, dir := newLocalStorageWithTemplate(t, slug, true)
	return &HubConnection{
		Name:         "hub-1",
		IsColocated:  true,
		LocalStorage: stor,
		HubClient: &stubHubClient{templates: &stubTemplateService{
			getFunc: func(ctx context.Context, ref string) (*hubclient.Template, error) {
				if onGet != nil {
					onGet()
				}
				if getErr != nil {
					return nil, getErr
				}
				return &hubclient.Template{ID: "tpl-uuid", Slug: slug, Scope: "global"}, nil
			},
		}},
	}, dir
}

// When the agent's broker-side state is gone, a start that carries the
// template identity hydrates the agent's own template and provisions from
// it, as create does.
func TestApplyStartTemplateIdentity_MissingStateHydratesTemplate(t *testing.T) {
	f := newTemplateIdentityFixture(t, "gone-agent", false)
	conn, wantDir := templateConn(t, "web-dev", nil, nil)

	err := f.apply(startContextInputs{TemplateID: "tpl-uuid", TemplateHash: "sha256:abc"}, conn, "web-dev")
	require.NoError(t, err)
	assert.Equal(t, wantDir, f.opts.Template, "the recreated agent must be provisioned from its own template")
}

// When the agent's broker-side state survived, the start is left exactly as
// it was: nothing is hydrated and opts.Template stays unset, so the existing
// agent resolves its image and harness-config as before. (That no env is
// added is checked over HTTP in TestStartAgent_SurvivingStateKeepsTemplateUnset.)
func TestApplyStartTemplateIdentity_SurvivingStateIsUnchanged(t *testing.T) {
	f := newTemplateIdentityFixture(t, "kept-agent", true)
	conn, _ := templateConn(t, "web-dev", nil, func() {
		t.Error("the template must not be hydrated when the agent's state survived")
	})

	err := f.apply(startContextInputs{TemplateID: "tpl-uuid", TemplateHash: "sha256:abc"}, conn, "web-dev")
	require.NoError(t, err)
	assert.Empty(t, f.opts.Template)
}

// A hydration failure while the agent has to be provisioned again fails the
// start with the same error create returns, rather than provisioning the
// agent from the default template.
func TestApplyStartTemplateIdentity_HydrationFailureFailsStart(t *testing.T) {
	f := newTemplateIdentityFixture(t, "gone-agent", false)
	conn, _ := templateConn(t, "web-dev", errors.New("hub unavailable"), nil)

	err := f.apply(startContextInputs{TemplateID: "tpl-uuid", TemplateHash: "sha256:abc"}, conn, "web-dev")
	var sce *startContextError
	require.ErrorAs(t, err, &sce)
	assert.Equal(t, http.StatusInternalServerError, sce.Status)
	assert.True(t, sce.IsHubError)
	assert.Empty(t, f.opts.Template)
}

// An older Hub sends no template identity: the start behaves as before (no
// hydration, no template path), and the Hub is never asked for a template.
func TestApplyStartTemplateIdentity_NoIdentityKeepsPreviousBehaviour(t *testing.T) {
	f := newTemplateIdentityFixture(t, "gone-agent", false)
	conn, _ := templateConn(t, "web-dev", nil, func() {
		t.Error("nothing may be hydrated without a template identity")
	})

	require.NoError(t, f.apply(startContextInputs{}, conn, "web-dev"))
	assert.Empty(t, f.opts.Template)
}

func TestRestartAgentConfig_NilWithoutInputs(t *testing.T) {
	assert.Nil(t, restartAgentConfig(false, "", "", "", nil, true, nil, nil, ""),
		"an older Hub's restart (sharedWorkspace alone, no project named) builds no config")
	cfg := restartAgentConfig(false, "claude", "", "", nil, false, nil, nil, "")
	require.NotNil(t, cfg)
	assert.Equal(t, "claude", cfg.HarnessConfig)

	// A request that names its project always builds the config, with
	// sharedWorkspace, as start does.
	cfg = restartAgentConfig(true, "", "", "", nil, true, nil, nil, "")
	require.NotNil(t, cfg)
	assert.True(t, cfg.SharedWorkspace)
}
