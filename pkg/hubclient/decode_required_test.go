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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// requiredBodyCase calls one hubclient method whose endpoint must return a
// body. It returns the call's error, or an error describing the non-nil
// result the call should not have produced.
type requiredBodyCase struct {
	name string
	call func(ctx context.Context, c Client) error
}

// expectNoResult turns a (result, err) pair into a single error: err if set,
// otherwise a failure that names the result returned alongside a nil error.
func expectNoResult[T any](v *T, err error) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("got result %+v with a nil error", v)
}

func expectNoValue[T any](v T, err error) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("got value %+v with a nil error", v)
}

func requiredBodyCases() []requiredBodyCase {
	return []requiredBodyCase{
		// ptone/scion#3688: env and credential-store calls.
		{"Env.List", func(ctx context.Context, c Client) error { return expectNoResult(c.Env().List(ctx, nil)) }},
		{"Env.Set", func(ctx context.Context, c Client) error {
			return expectNoResult(c.Env().Set(ctx, "K", &SetEnvRequest{Value: "v"}))
		}},
		{"Secrets.List", func(ctx context.Context, c Client) error { return expectNoResult(c.Secrets().List(ctx, nil)) }},
		{"Secrets.Set", func(ctx context.Context, c Client) error {
			return expectNoResult(c.Secrets().Set(ctx, "K", &SetSecretRequest{Value: "v"}))
		}},
		{"Secrets.UpdateMeta", func(ctx context.Context, c Client) error {
			return expectNoResult(c.Secrets().UpdateMeta(ctx, "K", &UpdateSecretMetaRequest{}))
		}},

		// Calls that dereferenced the decoded result inside hubclient.
		{"Agents.List", func(ctx context.Context, c Client) error { return expectNoResult(c.Agents().List(ctx, nil)) }},
		{"Agents.GetLogs", func(ctx context.Context, c Client) error { return expectNoValue(c.Agents().GetLogs(ctx, "a", nil)) }},
		{"Projects.List", func(ctx context.Context, c Client) error { return expectNoResult(c.Projects().List(ctx, nil)) }},
		{"Projects.ListAgents", func(ctx context.Context, c Client) error {
			return expectNoResult(c.Projects().ListAgents(ctx, "p", nil))
		}},
		{"RuntimeBrokers.List", func(ctx context.Context, c Client) error { return expectNoResult(c.RuntimeBrokers().List(ctx, nil)) }},
		{"Templates.List", func(ctx context.Context, c Client) error { return expectNoResult(c.Templates().List(ctx, nil)) }},
		{"Skills.List", func(ctx context.Context, c Client) error { return expectNoResult(c.Skills().List(ctx, nil)) }},
		{"HarnessConfigs.List", func(ctx context.Context, c Client) error { return expectNoResult(c.HarnessConfigs().List(ctx, nil)) }},
		{"HarnessConfigs.ReadFile", func(ctx context.Context, c Client) error {
			return expectNoValue(c.HarnessConfigs().ReadFile(ctx, "h", "f"))
		}},
		{"Users.List", func(ctx context.Context, c Client) error { return expectNoResult(c.Users().List(ctx, nil)) }},
		{"AllowList.ListDomains", func(ctx context.Context, c Client) error { return expectNoValue(c.AllowList().ListDomains(ctx)) }},
		{"Messages.ListChannels", func(ctx context.Context, c Client) error { return expectNoValue(c.Messages().ListChannels(ctx)) }},
		{"Artifacts.ListVersions", func(ctx context.Context, c Client) error { return expectNoValue(c.Artifacts().ListVersions(ctx, "a")) }},
		{"Subscriptions.BulkDelete", func(ctx context.Context, c Client) error {
			return expectNoValue(c.Subscriptions().BulkDelete(ctx, []string{"s"}))
		}},

		// One single-object call per remaining service.
		{"Agents.Get", func(ctx context.Context, c Client) error { return expectNoResult(c.Agents().Get(ctx, "a")) }},
		{"Agents.Create", func(ctx context.Context, c Client) error {
			return expectNoResult(c.Agents().Create(ctx, &CreateAgentRequest{Name: "a"}))
		}},
		{"Projects.Get", func(ctx context.Context, c Client) error { return expectNoResult(c.Projects().Get(ctx, "p")) }},
		{"RuntimeBrokers.Get", func(ctx context.Context, c Client) error { return expectNoResult(c.RuntimeBrokers().Get(ctx, "b")) }},
		{"Templates.Get", func(ctx context.Context, c Client) error { return expectNoResult(c.Templates().Get(ctx, "t")) }},
		{"Skills.Get", func(ctx context.Context, c Client) error { return expectNoResult(c.Skills().Get(ctx, "s")) }},
		{"SkillRegistries.Get", func(ctx context.Context, c Client) error { return expectNoResult(c.SkillRegistries().Get(ctx, "r")) }},
		{"HarnessConfigs.Get", func(ctx context.Context, c Client) error { return expectNoResult(c.HarnessConfigs().Get(ctx, "h")) }},
		{"Workspace.GetStatus", func(ctx context.Context, c Client) error { return expectNoResult(c.Workspace().GetStatus(ctx, "a")) }},
		{"Users.Get", func(ctx context.Context, c Client) error { return expectNoResult(c.Users().Get(ctx, "u")) }},
		{"Users.Provision", func(ctx context.Context, c Client) error {
			return expectNoResult(c.Users().Provision(ctx, &ProvisionUserRequest{Email: "a@example.com"}))
		}},
		{"Auth.Me", func(ctx context.Context, c Client) error { return expectNoResult(c.Auth().Me(ctx)) }},
		{"Tokens.List", func(ctx context.Context, c Client) error { return expectNoResult(c.Tokens().List(ctx)) }},
		{"Subscriptions.Create", func(ctx context.Context, c Client) error {
			return expectNoResult(c.Subscriptions().Create(ctx, &CreateSubscriptionRequest{}))
		}},
		{"SubscriptionTemplates.Create", func(ctx context.Context, c Client) error {
			return expectNoResult(c.SubscriptionTemplates().Create(ctx, &CreateSubscriptionTemplateRequest{}))
		}},
		{"ScheduledEvents.Get", func(ctx context.Context, c Client) error { return expectNoResult(c.ScheduledEvents("p").Get(ctx, "e")) }},
		{"Schedules.Get", func(ctx context.Context, c Client) error { return expectNoResult(c.Schedules("p").Get(ctx, "s")) }},
		{"GCPServiceAccounts.Get", func(ctx context.Context, c Client) error {
			return expectNoResult(c.GCPServiceAccounts().Get(ctx, HubScopedRef("sa")))
		}},
		{"Messages.Get", func(ctx context.Context, c Client) error { return expectNoResult(c.Messages().Get(ctx, "m")) }},
		{"Conversations.Get", func(ctx context.Context, c Client) error { return expectNoResult(c.Conversations().Get(ctx, "c")) }},
		{"Invites.Get", func(ctx context.Context, c Client) error { return expectNoResult(c.Invites().Get(ctx, "i")) }},
		{"Messaging.Capabilities", func(ctx context.Context, c Client) error { return expectNoResult(c.Messaging().Capabilities(ctx)) }},
		{"UserInjectedSkills.List", func(ctx context.Context, c Client) error { return expectNoResult(c.UserInjectedSkills().List(ctx)) }},
		{"ProjectPreStartHooks.Get", func(ctx context.Context, c Client) error {
			return expectNoResult(c.ProjectPreStartHooks("p").Get(ctx, "h"))
		}},
		{"HubPreStartHooks.Get", func(ctx context.Context, c Client) error { return expectNoResult(c.HubPreStartHooks().Get(ctx, "h")) }},
		{"DiscoverSkillsDirectory", func(ctx context.Context, c Client) error {
			return expectNoResult(c.DiscoverSkillsDirectory(ctx, DiscoverSkillsDirectoryRequest{}))
		}},
		{"Artifacts.Get", func(ctx context.Context, c Client) error { return expectNoResult(c.Artifacts().Get(ctx, "a")) }},
	}
}

// TestRequiredBody_EmptyResponse checks that calls whose endpoints must
// return a body report apiclient.ErrNoContent on a 204 or an empty 200,
// instead of returning a nil result or dereferencing one.
func TestRequiredBody_EmptyResponse(t *testing.T) {
	responses := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"204", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}},
		{"empty 200", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
		}},
	}

	for _, resp := range responses {
		t.Run(resp.name, func(t *testing.T) {
			server := httptest.NewServer(resp.handler)
			t.Cleanup(server.Close)
			client, err := New(server.URL)
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			for _, tc := range requiredBodyCases() {
				t.Run(tc.name, func(t *testing.T) {
					var err error
					func() {
						defer func() {
							if r := recover(); r != nil {
								err = fmt.Errorf("panicked: %v", r)
							}
						}()
						err = tc.call(t.Context(), client)
					}()
					if !errors.Is(err, apiclient.ErrNoContent) {
						t.Fatalf("error = %v, want one wrapping apiclient.ErrNoContent", err)
					}
				})
			}
		})
	}
}
