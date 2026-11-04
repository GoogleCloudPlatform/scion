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

package hubclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every caller-supplied value a method puts into a URL path is escaped with
// url.PathEscape (#6879): an ID containing "/" or "?" stays one path segment
// and never starts a query. Fixed path literals are not escaped.

// pathEscapeIDs are the IDs each case runs with. want is the escaped form
// expected in the request path. The UUID and slug are controls: normal IDs
// are sent unchanged.
var pathEscapeIDs = []struct {
	name, id, want string
}{
	{"slash", "a/b", "a%2Fb"},
	{"question mark", "x?y", "x%3Fy"},
	{"uuid control", "0f8fad5b-d9cb-469f-a165-70867728950e", "0f8fad5b-d9cb-469f-a165-70867728950e"},
	{"slug control", "my-slug", "my-slug"},
}

// pathEscapeCase is one method under test. want is the expected escaped
// request path, with every {id} replaced by the escaped ID. call passes the
// same ID for every caller-supplied path value of the method.
type pathEscapeCase struct {
	name string
	want string
	call func(ctx context.Context, c Client, id string)
}

// recordFirstPath starts a stand-in hub that answers every request with
// 200 and an empty JSON object, runs call against it and returns the escaped
// path of the first request. The method's own result is ignored: only the
// request path is under test.
func recordFirstPath(t *testing.T, call func(ctx context.Context, c Client)) string {
	t.Helper()
	var (
		mu    sync.Mutex
		paths []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.EscapedPath())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(server.Close)

	client, err := New(server.URL)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	call(ctx, client)

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, paths, "the method sent no request")
	return paths[0]
}

func runPathEscapeCases(t *testing.T, cases []pathEscapeCase) {
	t.Helper()
	for _, tc := range cases {
		for _, id := range pathEscapeIDs {
			t.Run(tc.name+"/"+id.name, func(t *testing.T) {
				got := recordFirstPath(t, func(ctx context.Context, c Client) { tc.call(ctx, c, id.id) })
				assert.Equal(t, strings.ReplaceAll(tc.want, "{id}", id.want), got)
			})
		}
	}
}

func TestPathEscape_Agents(t *testing.T) {
	msg := &messages.StructuredMessage{Msg: "hi"}
	// agentCases covers the hub-scoped agent service; each case runs again
	// on the project-scoped service, where the project ID is escaped too.
	agentCases := []struct {
		name string
		tail string // path after the agent ID
		call func(ctx context.Context, s AgentService, id string)
	}{
		{"Get", "", func(ctx context.Context, s AgentService, id string) { _, _ = s.Get(ctx, id) }},
		{"Update", "", func(ctx context.Context, s AgentService, id string) { _, _ = s.Update(ctx, id, &UpdateAgentRequest{}) }},
		{"Delete", "", func(ctx context.Context, s AgentService, id string) { _ = s.Delete(ctx, id, nil) }},
		{"DeleteWithResult", "", func(ctx context.Context, s AgentService, id string) {
			_, _ = s.(AgentDeleteResulter).DeleteWithResult(ctx, id, nil)
		}},
		{"Start", "/start", func(ctx context.Context, s AgentService, id string) { _, _ = s.Start(ctx, id) }},
		{"StartWithOptions", "/start", func(ctx context.Context, s AgentService, id string) {
			_, _ = s.(*agentService).StartWithOptions(ctx, id, LifecycleStartOptions{})
		}},
		{"Stop", "/stop", func(ctx context.Context, s AgentService, id string) { _, _ = s.Stop(ctx, id) }},
		{"Suspend", "/suspend", func(ctx context.Context, s AgentService, id string) { _, _ = s.Suspend(ctx, id) }},
		{"Restart", "/restart", func(ctx context.Context, s AgentService, id string) { _, _ = s.Restart(ctx, id) }},
		{"RestartWithOptions", "/restart", func(ctx context.Context, s AgentService, id string) {
			_, _ = s.(*agentService).RestartWithOptions(ctx, id, LifecycleStartOptions{})
		}},
		{"Restore", "/restore", func(ctx context.Context, s AgentService, id string) { _, _ = s.Restore(ctx, id) }},
		{"ResetAuth", "/reset-auth", func(ctx context.Context, s AgentService, id string) { _ = s.ResetAuth(ctx, id) }},
		{"ReissueScopes", "/reset-auth", func(ctx context.Context, s AgentService, id string) {
			_, _ = s.(*agentService).ReissueScopes(ctx, id, true)
		}},
		{"SubmitEnv", "/env", func(ctx context.Context, s AgentService, id string) { _, _ = s.SubmitEnv(ctx, id, &SubmitEnvRequest{}) }},
		{"SendMessage", "/message", func(ctx context.Context, s AgentService, id string) { _ = s.SendMessage(ctx, id, "hi", false) }},
		{"SendStructuredMessageWithOptions", "/message", func(ctx context.Context, s AgentService, id string) {
			_, _ = s.SendStructuredMessageWithOptions(ctx, id, msg, SendMessageOptions{})
		}},
		{"SendOutboundMessage", "/outbound-message", func(ctx context.Context, s AgentService, id string) {
			_, _ = s.SendOutboundMessage(ctx, id, &OutboundMessageRequest{})
		}},
		{"SendKeys", "/keys", func(ctx context.Context, s AgentService, id string) { _, _ = s.SendKeys(ctx, id, "ls") }},
		{"Exec", "/exec", func(ctx context.Context, s AgentService, id string) { _, _ = s.Exec(ctx, id, []string{"true"}, 0) }},
		{"GetLogs", "/logs", func(ctx context.Context, s AgentService, id string) { _, _ = s.GetLogs(ctx, id, nil) }},
		{"GetCloudLogs", "/cloud-logs", func(ctx context.Context, s AgentService, id string) { _, _ = s.GetCloudLogs(ctx, id, nil) }},
		{"StreamCloudLogs", "/cloud-logs/stream", func(ctx context.Context, s AgentService, id string) {
			_ = s.StreamCloudLogs(ctx, id, nil, func(CloudLogEntry) {})
		}},
		{"SetMessageMode", "/set_message_mode", func(ctx context.Context, s AgentService, id string) {
			_, _ = s.SetMessageMode(ctx, id, &SetMessageModeRequest{}, nil)
		}},
		{"Reincarnate", "/reincarnate", func(ctx context.Context, s AgentService, id string) {
			_, _ = s.Reincarnate(ctx, id, &ReincarnateAgentRequest{})
		}},
	}

	var cases []pathEscapeCase
	for _, ac := range agentCases {
		cases = append(cases,
			pathEscapeCase{"hub/" + ac.name, "/api/v1/agents/{id}" + ac.tail,
				func(ctx context.Context, c Client, id string) { ac.call(ctx, c.Agents(), id) }},
			pathEscapeCase{"project/" + ac.name, "/api/v1/projects/{id}/agents/{id}" + ac.tail,
				func(ctx context.Context, c Client, id string) { ac.call(ctx, c.ProjectAgents(id), id) }},
		)
	}
	// Methods whose only caller-supplied path value is the project ID.
	cases = append(cases,
		pathEscapeCase{"project/List", "/api/v1/projects/{id}/agents",
			func(ctx context.Context, c Client, id string) { _, _ = c.ProjectAgents(id).List(ctx, nil) }},
		pathEscapeCase{"project/Create", "/api/v1/projects/{id}/agents",
			func(ctx context.Context, c Client, id string) {
				_, _ = c.ProjectAgents(id).Create(ctx, &CreateAgentRequest{Name: "n"})
			}},
		pathEscapeCase{"project/StopAll", "/api/v1/projects/{id}/agents/stop-all",
			func(ctx context.Context, c Client, id string) { _, _ = c.ProjectAgents(id).StopAll(ctx) }},
		pathEscapeCase{"project/BroadcastMessage", "/api/v1/projects/{id}/broadcast",
			func(ctx context.Context, c Client, id string) {
				_, _ = c.ProjectAgents(id).BroadcastMessage(ctx, msg, false)
			}},
	)
	runPathEscapeCases(t, cases)
}

func TestPathEscape_ProjectScopedServices(t *testing.T) {
	runPathEscapeCases(t, []pathEscapeCase{
		{"InjectedSkills/List", "/api/v1/projects/{id}/injected-skills",
			func(ctx context.Context, c Client, id string) { _, _ = c.ProjectInjectedSkills(id).List(ctx) }},
		{"InjectedSkills/Add", "/api/v1/projects/{id}/injected-skills",
			func(ctx context.Context, c Client, id string) {
				_, _ = c.ProjectInjectedSkills(id).Add(ctx, &AddInjectedSkillRequest{})
			}},
		{"InjectedSkills/Set", "/api/v1/projects/{id}/injected-skills",
			func(ctx context.Context, c Client, id string) { _, _ = c.ProjectInjectedSkills(id).Set(ctx, nil) }},
		{"InjectedSkills/Remove", "/api/v1/projects/{id}/injected-skills/{id}",
			func(ctx context.Context, c Client, id string) { _ = c.ProjectInjectedSkills(id).Remove(ctx, id) }},
		{"PreStartHooks/List", "/api/v1/projects/{id}/pre-start-hooks",
			func(ctx context.Context, c Client, id string) { _, _ = c.ProjectPreStartHooks(id).List(ctx) }},
		{"PreStartHooks/Get", "/api/v1/projects/{id}/pre-start-hooks/{id}",
			func(ctx context.Context, c Client, id string) { _, _ = c.ProjectPreStartHooks(id).Get(ctx, id) }},
		{"PreStartHooks/Create", "/api/v1/projects/{id}/pre-start-hooks",
			func(ctx context.Context, c Client, id string) {
				_, _ = c.ProjectPreStartHooks(id).Create(ctx, &CreateProjectPreStartHookRequest{})
			}},
		{"PreStartHooks/Update", "/api/v1/projects/{id}/pre-start-hooks/{id}",
			func(ctx context.Context, c Client, id string) {
				_, _ = c.ProjectPreStartHooks(id).Update(ctx, id, &UpdateProjectPreStartHookRequest{})
			}},
		{"PreStartHooks/Activate", "/api/v1/projects/{id}/pre-start-hooks/{id}/activate",
			func(ctx context.Context, c Client, id string) { _, _ = c.ProjectPreStartHooks(id).Activate(ctx, id) }},
		{"PreStartHooks/Delete", "/api/v1/projects/{id}/pre-start-hooks/{id}",
			func(ctx context.Context, c Client, id string) { _ = c.ProjectPreStartHooks(id).Delete(ctx, id) }},
	})
}

func TestPathEscape_GCPServiceAccounts(t *testing.T) {
	runPathEscapeCases(t, []pathEscapeCase{
		{"Get/hub", "/api/v1/gcp-service-accounts/{id}",
			func(ctx context.Context, c Client, id string) {
				_, _ = c.GCPServiceAccounts().Get(ctx, HubScopedRef(id))
			}},
		{"Get/project", "/api/v1/projects/{id}/gcp-service-accounts/{id}",
			func(ctx context.Context, c Client, id string) {
				_, _ = c.GCPServiceAccounts().Get(ctx, ProjectScopedRef(id, id))
			}},
		{"Delete/project", "/api/v1/projects/{id}/gcp-service-accounts/{id}",
			func(ctx context.Context, c Client, id string) {
				_, _ = c.GCPServiceAccounts().Delete(ctx, ProjectScopedRef(id, id), nil)
			}},
		{"Verify/hub", "/api/v1/gcp-service-accounts/{id}/verify",
			func(ctx context.Context, c Client, id string) {
				_, _ = c.GCPServiceAccounts().Verify(ctx, HubScopedRef(id))
			}},
		{"Verify/project", "/api/v1/projects/{id}/gcp-service-accounts/{id}/verify",
			func(ctx context.Context, c Client, id string) {
				_, _ = c.GCPServiceAccounts().Verify(ctx, ProjectScopedRef(id, id))
			}},
		{"Status", "/api/v1/projects/{id}/gcp-service-accounts/{id}/status",
			func(ctx context.Context, c Client, id string) { _, _ = c.GCPServiceAccounts().Status(ctx, id, id) }},
		{"Mint", "/api/v1/projects/{id}/gcp-service-accounts/mint",
			func(ctx context.Context, c Client, id string) {
				_, _ = c.GCPServiceAccounts().Mint(ctx, id, &MintGCPServiceAccountRequest{})
			}},
	})
}

func TestPathEscape_Users(t *testing.T) {
	runPathEscapeCases(t, []pathEscapeCase{
		{"Get", "/api/v1/users/{id}",
			func(ctx context.Context, c Client, id string) { _, _ = c.Users().Get(ctx, id) }},
		{"Update", "/api/v1/users/{id}",
			func(ctx context.Context, c Client, id string) { _, _ = c.Users().Update(ctx, id, &UpdateUserRequest{}) }},
	})
}

func TestPathEscape_Projects(t *testing.T) {
	p := func(c Client) ProjectService { return c.Projects() }
	runPathEscapeCases(t, []pathEscapeCase{
		{"Get", "/api/v1/projects/{id}",
			func(ctx context.Context, c Client, id string) { _, _ = p(c).Get(ctx, id) }},
		{"Update", "/api/v1/projects/{id}",
			func(ctx context.Context, c Client, id string) { _, _ = p(c).Update(ctx, id, &UpdateProjectRequest{}) }},
		{"Delete", "/api/v1/projects/{id}",
			func(ctx context.Context, c Client, id string) {
				_ = p(c).Delete(ctx, id, &DeleteProjectOptions{Force: true})
			}},
		{"ListAgents", "/api/v1/projects/{id}/agents",
			func(ctx context.Context, c Client, id string) { _, _ = p(c).ListAgents(ctx, id, nil) }},
		{"ListProviders", "/api/v1/projects/{id}/providers",
			func(ctx context.Context, c Client, id string) { _, _ = p(c).ListProviders(ctx, id) }},
		{"AddProvider", "/api/v1/projects/{id}/providers",
			func(ctx context.Context, c Client, id string) {
				_, _ = p(c).AddProvider(ctx, id, &AddProviderRequest{})
			}},
		{"RemoveProvider", "/api/v1/projects/{id}/providers/{id}",
			func(ctx context.Context, c Client, id string) { _ = p(c).RemoveProvider(ctx, id, id) }},
		{"GetSettings", "/api/v1/projects/{id}/settings",
			func(ctx context.Context, c Client, id string) { _, _ = p(c).GetSettings(ctx, id) }},
		{"UpdateSettings", "/api/v1/projects/{id}/settings",
			func(ctx context.Context, c Client, id string) {
				_, _ = p(c).UpdateSettings(ctx, id, &ProjectSettings{})
			}},
		{"GetResolvedSettings", "/api/v1/projects/{id}/settings/resolved",
			func(ctx context.Context, c Client, id string) { _, _ = p(c).GetResolvedSettings(ctx, id) }},
		{"GetSettingsBody", "/api/v1/projects/{id}/settings",
			func(ctx context.Context, c Client, id string) { _, _ = p(c).GetSettingsBody(ctx, id) }},
		{"GetResolvedSettingsBody", "/api/v1/projects/{id}/settings/resolved",
			func(ctx context.Context, c Client, id string) { _, _ = p(c).GetResolvedSettingsBody(ctx, id) }},
		{"UpdateSettingsFields", "/api/v1/projects/{id}/settings",
			func(ctx context.Context, c Client, id string) {
				_, _ = p(c).UpdateSettingsFields(ctx, id, map[string]interface{}{"defaultModel": "m"})
			}},
		{"UpdateGCPIdentityDefaults", "/api/v1/projects/{id}/settings",
			func(ctx context.Context, c Client, id string) {
				_, _ = p(c).UpdateGCPIdentityDefaults(ctx, id, &ProjectGCPIdentityDefaultsUpdate{})
			}},
		{"GetAgent", "/api/v1/projects/{id}/agents/{id}",
			func(ctx context.Context, c Client, id string) { _, _ = p(c).GetAgent(ctx, id, id) }},
		{"DeleteAgent", "/api/v1/projects/{id}/agents/{id}",
			func(ctx context.Context, c Client, id string) { _ = p(c).DeleteAgent(ctx, id, id, nil) }},
		{"Clone", "/api/v1/projects/{id}/clone",
			func(ctx context.Context, c Client, id string) { _, _ = p(c).Clone(ctx, id, CloneProjectRequest{}) }},
	})
}

func TestPathEscape_Templates(t *testing.T) {
	tm := func(c Client) TemplateService { return c.Templates() }
	runPathEscapeCases(t, []pathEscapeCase{
		{"Get", "/api/v1/templates/{id}",
			func(ctx context.Context, c Client, id string) { _, _ = tm(c).Get(ctx, id) }},
		{"Delete", "/api/v1/templates/{id}",
			func(ctx context.Context, c Client, id string) { _ = tm(c).Delete(ctx, id) }},
		{"Clone", "/api/v1/templates/{id}/clone",
			func(ctx context.Context, c Client, id string) { _, _ = tm(c).Clone(ctx, id, &CloneTemplateRequest{}) }},
		{"Reimport", "/api/v1/templates/{id}/reimport",
			func(ctx context.Context, c Client, id string) { _, _ = tm(c).Reimport(ctx, id, "") }},
		{"RequestUploadURLs", "/api/v1/templates/{id}/upload",
			func(ctx context.Context, c Client, id string) { _, _ = tm(c).RequestUploadURLs(ctx, id, nil) }},
		{"Finalize", "/api/v1/templates/{id}/finalize",
			func(ctx context.Context, c Client, id string) {
				_, _ = tm(c).Finalize(ctx, id, &TemplateManifest{}, "", "")
			}},
		{"RequestDownloadURLs", "/api/v1/templates/{id}/download",
			func(ctx context.Context, c Client, id string) { _, _ = tm(c).RequestDownloadURLs(ctx, id) }},
		{"Validate", "/api/v1/templates/{id}/validate",
			func(ctx context.Context, c Client, id string) { _, _ = tm(c).Validate(ctx, id) }},
	})
}

func TestPathEscape_Skills(t *testing.T) {
	sk := func(c Client) SkillService { return c.Skills() }
	runPathEscapeCases(t, []pathEscapeCase{
		{"Get", "/api/v1/skills/{id}",
			func(ctx context.Context, c Client, id string) { _, _ = sk(c).Get(ctx, id) }},
		{"Update", "/api/v1/skills/{id}",
			func(ctx context.Context, c Client, id string) { _, _ = sk(c).Update(ctx, id, &UpdateSkillRequest{}) }},
		{"Delete", "/api/v1/skills/{id}",
			func(ctx context.Context, c Client, id string) { _ = sk(c).Delete(ctx, id) }},
		{"PublishVersion", "/api/v1/skills/{id}/versions",
			func(ctx context.Context, c Client, id string) {
				_, _ = sk(c).PublishVersion(ctx, id, &PublishVersionRequest{})
			}},
		{"ListVersions", "/api/v1/skills/{id}/versions",
			func(ctx context.Context, c Client, id string) { _, _ = sk(c).ListVersions(ctx, id) }},
		{"FinalizeVersion", "/api/v1/skills/{id}/finalize",
			func(ctx context.Context, c Client, id string) {
				_, _ = sk(c).FinalizeVersion(ctx, id, &FinalizeSkillVersionRequest{})
			}},
		{"RequestUploadURLs", "/api/v1/skills/{id}/upload",
			func(ctx context.Context, c Client, id string) { _, _ = sk(c).RequestUploadURLs(ctx, id, "1.0.0", nil) }},
		{"DeprecateVersion", "/api/v1/skills/{id}/versions/{id}/deprecate",
			func(ctx context.Context, c Client, id string) {
				_, _ = sk(c).DeprecateVersion(ctx, id, id, &DeprecateVersionRequest{})
			}},
	})
}

func TestPathEscape_HarnessConfigs(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = root.Close() })

	hc := func(c Client) HarnessConfigService { return c.HarnessConfigs() }
	runPathEscapeCases(t, []pathEscapeCase{
		{"Get", "/api/v1/harness-configs/{id}",
			func(ctx context.Context, c Client, id string) { _, _ = hc(c).Get(ctx, id) }},
		{"Update", "/api/v1/harness-configs/{id}",
			func(ctx context.Context, c Client, id string) {
				_, _ = hc(c).Update(ctx, id, &UpdateHarnessConfigRequest{})
			}},
		{"Delete", "/api/v1/harness-configs/{id}",
			func(ctx context.Context, c Client, id string) { _ = hc(c).Delete(ctx, id) }},
		{"RequestUploadURLs", "/api/v1/harness-configs/{id}/upload",
			func(ctx context.Context, c Client, id string) { _, _ = hc(c).RequestUploadURLs(ctx, id, nil) }},
		{"Finalize", "/api/v1/harness-configs/{id}/finalize",
			func(ctx context.Context, c Client, id string) {
				_, _ = hc(c).Finalize(ctx, id, &HarnessConfigManifest{}, "", "")
			}},
		{"RequestDownloadURLs", "/api/v1/harness-configs/{id}/download",
			func(ctx context.Context, c Client, id string) { _, _ = hc(c).RequestDownloadURLs(ctx, id) }},
		{"UploadFilesMultipart", "/api/v1/harness-configs/{id}/files",
			func(ctx context.Context, c Client, id string) { _ = hc(c).UploadFilesMultipart(ctx, id, root, nil) }},
		// The file path keeps its "/" separators: escapePathSegments
		// escapes each segment once, and the ID is escaped once.
		{"ReadFile", "/api/v1/harness-configs/{id}/files/dir/f%3Fx.md",
			func(ctx context.Context, c Client, id string) { _, _ = hc(c).ReadFile(ctx, id, "dir/f?x.md") }},
		{"Reimport", "/api/v1/harness-configs/{id}/reimport",
			func(ctx context.Context, c Client, id string) { _, _ = hc(c).Reimport(ctx, id, "") }},
		{"Validate", "/api/v1/harness-configs/{id}/validate",
			func(ctx context.Context, c Client, id string) { _, _ = hc(c).Validate(ctx, id) }},
	})
}

func TestPathEscape_SkillRegistries(t *testing.T) {
	runPathEscapeCases(t, []pathEscapeCase{
		{"Get", "/api/v1/skill-registries/{id}",
			func(ctx context.Context, c Client, id string) { _, _ = c.SkillRegistries().Get(ctx, id) }},
		{"Update", "/api/v1/skill-registries/{id}",
			func(ctx context.Context, c Client, id string) {
				_, _ = c.SkillRegistries().Update(ctx, id, &UpdateSkillRegistryRequest{})
			}},
		{"Delete", "/api/v1/skill-registries/{id}",
			func(ctx context.Context, c Client, id string) { _ = c.SkillRegistries().Delete(ctx, id) }},
		{"Pin", "/api/v1/skill-registries/{id}/pin",
			func(ctx context.Context, c Client, id string) {
				_ = c.SkillRegistries().Pin(ctx, id, &PinSkillHashRequest{})
			}},
	})
}

func TestPathEscape_RuntimeBrokers(t *testing.T) {
	rb := func(c Client) RuntimeBrokerService { return c.RuntimeBrokers() }
	runPathEscapeCases(t, []pathEscapeCase{
		{"Get", "/api/v1/runtime-brokers/{id}",
			func(ctx context.Context, c Client, id string) { _, _ = rb(c).Get(ctx, id) }},
		{"Update", "/api/v1/runtime-brokers/{id}",
			func(ctx context.Context, c Client, id string) { _, _ = rb(c).Update(ctx, id, &UpdateBrokerRequest{}) }},
		{"Delete", "/api/v1/runtime-brokers/{id}",
			func(ctx context.Context, c Client, id string) { _ = rb(c).Delete(ctx, id) }},
		{"ListProjects", "/api/v1/runtime-brokers/{id}/projects",
			func(ctx context.Context, c Client, id string) { _, _ = rb(c).ListProjects(ctx, id) }},
		{"Heartbeat", "/api/v1/runtime-brokers/{id}/heartbeat",
			func(ctx context.Context, c Client, id string) { _, _ = rb(c).Heartbeat(ctx, id, &BrokerHeartbeat{}) }},
	})
}

func TestPathEscape_Tokens(t *testing.T) {
	runPathEscapeCases(t, []pathEscapeCase{
		{"Get", "/api/v1/auth/tokens/{id}",
			func(ctx context.Context, c Client, id string) { _, _ = c.Tokens().Get(ctx, id) }},
		{"Revoke", "/api/v1/auth/tokens/{id}/revoke",
			func(ctx context.Context, c Client, id string) { _ = c.Tokens().Revoke(ctx, id) }},
		{"Delete", "/api/v1/auth/tokens/{id}",
			func(ctx context.Context, c Client, id string) { _ = c.Tokens().Delete(ctx, id) }},
	})
}

func TestPathEscape_Workspace(t *testing.T) {
	runPathEscapeCases(t, []pathEscapeCase{
		{"SyncFrom", "/api/v1/agents/{id}/workspace/sync-from",
			func(ctx context.Context, c Client, id string) { _, _ = c.Workspace().SyncFrom(ctx, id, nil) }},
		{"SyncTo", "/api/v1/agents/{id}/workspace/sync-to",
			func(ctx context.Context, c Client, id string) {
				_, _ = c.Workspace().SyncTo(ctx, id, []transfer.FileInfo{})
			}},
		{"FinalizeSyncTo", "/api/v1/agents/{id}/workspace/sync-to/finalize",
			func(ctx context.Context, c Client, id string) {
				_, _ = c.Workspace().FinalizeSyncTo(ctx, id, &transfer.Manifest{})
			}},
		{"GetStatus", "/api/v1/agents/{id}/workspace",
			func(ctx context.Context, c Client, id string) { _, _ = c.Workspace().GetStatus(ctx, id) }},
	})
}
