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

// Provision credentials (ptone/scion#2129).
//
// The broker uses ProvisionCredentials only as GitHub bearer credentials
// for the agent's gh:// skills (agent.GitHubSkillResolver); they never
// reach the container. The hub sends only the project-scope secrets those
// skills can use, each authorized for delivery to the agent under the
// delivery credential before its value is read. A secret named only by a
// ?token= reference is sent only when its value is shaped like a GitHub
// token (githubTokenShaped).

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/ghauth"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ghCredentialKeysForSkills returns the secret names the broker's GitHub
// skill resolver can look up for refs, sorted and without duplicates, and
// the subset of those names that only a ?token= reference names (tokenOnly;
// nil when there is none). It
// follows the resolver's lookup (GitHubSkillResolver.lookupToken and
// NewGitHubSkillResolverWithCredentials) for every ref the broker routes to
// it (gh:// and github.com URLs, agent.ParseGitHubSkillURI):
//
//   - a ref with ?token=NAME uses only NAME;
//   - any other ref uses its repo key GH_{OWNER}__{REPO}, its owner key
//     GH_{OWNER} and, when the broker has no other default token,
//     GITHUB_TOKEN.
//
// Refs that are not GitHub skill refs add nothing, so an agent with no
// gh:// refs gets no keys.
//
// A name that is also a convention key of a bare ref (GH_*, GITHUB_TOKEN)
// is not in tokenOnly: the broker uses it for that ref whatever its value.
func ghCredentialKeysForSkills(refs []api.SkillReference) (keys []string, tokenOnly map[string]bool) {
	named := make(map[string]bool)
	convention := make(map[string]bool)
	for _, ref := range refs {
		gh, err := agent.ParseGitHubSkillURI(ref.URI)
		if err != nil {
			continue
		}
		if gh.TokenSecretName != "" {
			named[gh.TokenSecretName] = true
			continue
		}
		convention[ghauth.RepoKey(gh.Owner, gh.Repo)] = true
		convention[ghauth.OwnerKey(gh.Owner)] = true
		convention[ghauth.DefaultKey] = true
	}
	for k := range convention {
		keys = append(keys, k)
	}
	for k := range named {
		if convention[k] {
			continue
		}
		keys = append(keys, k)
		if tokenOnly == nil {
			tokenOnly = make(map[string]bool)
		}
		tokenOnly[k] = true
	}
	sort.Strings(keys)
	return keys, tokenOnly
}

// githubTokenPrefixes are the documented prefixes of GitHub tokens:
// personal access tokens (classic ghp_, fine-grained github_pat_), OAuth
// (gho_), GitHub App user-to-server (ghu_), installation (ghs_) and refresh
// (ghr_) tokens.
var githubTokenPrefixes = []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_"}

// githubTokenShaped reports whether v looks like a GitHub token: a
// documented prefix followed by one or more ASCII letters, digits or
// underscores, and nothing else.
func githubTokenShaped(v string) bool {
	for _, p := range githubTokenPrefixes {
		rest, ok := strings.CutPrefix(v, p)
		if !ok {
			continue
		}
		if rest == "" {
			return false
		}
		for i := 0; i < len(rest); i++ {
			c := rest[i]
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' {
				return false
			}
		}
		return true
	}
	return false
}

// resolveProvisionCredentials returns the provision credentials for agent:
// its project-scope secrets whose names are in the key set of its merged
// skill references (ghCredentialKeysForSkills), each one authorized as a
// project-association delivery to the agent (Decide(secret.deliver) under
// the delivery credential) before any value is read. Values are then
// fetched by recorded metadata (FetchValues). A secret named only by a
// ?token= reference is then dropped unless its value is GitHub-token shaped
// (githubTokenShaped), so such a reference cannot carry an arbitrary
// project secret to the broker. User-scope secrets are never sent. With no
// gh:// references, nothing is listed, authorized or sent.
//
// It is shared by the create, start and restart paths, so a gh:// skill
// re-provisioned through any of them gets the same credentials. Failures
// do not fail the dispatch: they are logged at Warn and the affected
// credentials are left out, so the skill resolves with the default
// credential or none; once ctx has ended those Warns are not written, since
// the failure is then the caller's cancel or deadline. callerName prefixes
// every log message this function writes, at any level; no value is ever
// logged.
func (d *HTTPAgentDispatcher) resolveProvisionCredentials(ctx context.Context, agent *store.Agent, callerName string) map[string]string {
	if agent == nil || agent.ProjectID == "" || d.secretBackend == nil {
		return nil
	}
	var keys []string
	var tokenOnly map[string]bool
	if d.skillRefLister != nil {
		keys, tokenOnly = ghCredentialKeysForSkills(d.skillRefLister(ctx, agent))
	}
	if len(keys) == 0 {
		d.log.Info(callerName+": ProvisionCredentials resolved",
			"agent_id", agent.ID, "project_id", agent.ProjectID, "count", 0)
		return nil
	}
	wanted := make(map[string]bool, len(keys))
	for _, k := range keys {
		wanted[k] = true
	}

	projectSecrets, listErr := d.secretBackend.List(ctx, secret.Filter{
		Scope:   secret.ScopeProject,
		ScopeID: agent.ProjectID,
	})
	if listErr != nil {
		// Logged regardless of debug: without these credentials a private
		// gh:// skill silently resolves with the default credential (or none).
		// Not logged once the request has ended: that failure is the caller's
		// cancel or deadline, not a backend fault.
		if ctx.Err() == nil {
			d.log.Warn(callerName+": failed to list project secrets for ProvisionCredentials",
				"agent_id", agent.ID, "project_id", agent.ProjectID, "error", listErr)
		}
		return nil
	}

	sel := newDeliverySelector(d.authzService, agent, PurposeProvisionCredential)
	var kept []secret.SecretMeta
	var refused []string
	for _, m := range projectSecrets {
		if m.SecretType == store.SecretTypeInternal || !wanted[m.Name] {
			continue
		}
		allowed, err := sel.authorize(ctx, MaterialKindSecret, materialRecordSecret, m.ID, m.Name, store.ScopeProject, agent.ProjectID)
		if err != nil {
			if ctx.Err() == nil {
				d.log.Warn(callerName+": ProvisionCredentials not authorized; none sent",
					"agent_id", agent.ID, "project_id", agent.ProjectID, "error", err)
			}
			return nil
		}
		if allowed {
			kept = append(kept, m)
		} else {
			refused = append(refused, m.Name)
		}
	}
	if len(refused) > 0 {
		// Names only: a refused secret that is set would otherwise look,
		// from the broker's skill error, as if it were missing.
		sort.Strings(refused)
		d.log.Warn(callerName+": ProvisionCredentials not authorized for delivery to the agent; not sent",
			"agent_id", agent.ID, "project_id", agent.ProjectID, "secrets", refused)
	}
	if len(kept) == 0 {
		d.log.Info(callerName+": ProvisionCredentials resolved",
			"agent_id", agent.ID, "project_id", agent.ProjectID, "count", 0)
		return nil
	}

	values, fetchErr := d.secretBackend.FetchValues(ctx, kept)
	if fetchErr != nil {
		if ctx.Err() == nil {
			d.log.Warn(callerName+": failed to fetch project secrets for ProvisionCredentials",
				"agent_id", agent.ID, "project_id", agent.ProjectID, "error", fetchErr)
		}
		return nil
	}

	creds := make(map[string]string, len(kept))
	// interrupted counts secrets not fetched because ctx was cancelled or
	// timed out. Those are reported once below rather than once per secret.
	interrupted := 0
	// interruptCause is the first per-item context error, reported when ctx
	// itself has not ended.
	var interruptCause error
	for _, m := range kept {
		r, ok := values[m.ID]
		if !ok {
			if ctx.Err() != nil {
				interrupted++
				continue
			}
			// FetchValues returns every requested ID; a backend that does
			// not still gets the secret reported, by name only.
			d.log.Warn(callerName+": failed to get project secret for ProvisionCredentials",
				"agent_id", agent.ID, "project_id", agent.ProjectID, "secret", m.Name, "error", "no result returned")
			continue
		}
		if r.Err != nil {
			if ctx.Err() != nil || errors.Is(r.Err, context.Canceled) || errors.Is(r.Err, context.DeadlineExceeded) {
				interrupted++
				if interruptCause == nil {
					interruptCause = r.Err
				}
				continue
			}
			// Secret name and project only; never the value.
			d.log.Warn(callerName+": failed to get project secret for ProvisionCredentials",
				"agent_id", agent.ID, "project_id", agent.ProjectID, "secret", m.Name, "error", r.Err)
			continue
		}
		if r.Value == "" {
			continue
		}
		if tokenOnly[m.Name] && !githubTokenShaped(r.Value) {
			// Secret name only; never the value or any part of it.
			d.log.Warn(callerName+": ProvisionCredentials ?token= secret is not a GitHub token; not sent",
				"agent_id", agent.ID, "project_id", agent.ProjectID, "secret", m.Name)
			continue
		}
		creds[m.Name] = r.Value
	}
	if interrupted > 0 {
		cause := ctx.Err()
		if cause == nil {
			cause = interruptCause
		}
		d.log.Warn(callerName+": ProvisionCredentials fetch stopped early; some project secrets were not fetched",
			"agent_id", agent.ID, "project_id", agent.ProjectID, "not_fetched", interrupted, "error", cause)
	}
	d.log.Info(callerName+": ProvisionCredentials resolved",
		"agent_id", agent.ID, "project_id", agent.ProjectID, "count", len(creds))
	if len(creds) == 0 {
		return nil
	}
	return creds
}
