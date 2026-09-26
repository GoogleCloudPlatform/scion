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
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
)

// Hub-side skill resolution at dispatch (#1784).
//
// The runtime broker used to resolve every required skill itself, calling
// POST /api/v1/skills/resolve with its own HMAC identity. The authorization
// kernel denies all broker principals, so any non-public skill failed to
// provision regardless of who created the agent. Skill visibility is a policy
// decision that belongs at the Hub, which already knows who is creating the
// agent: the dispatcher now resolves the agent's Hub-registry skill references
// as that principal and ships the result (versions, content hashes, download
// URLs) to the broker in RemoteCreateAgentRequest.PreResolvedSkills. The broker
// installs pre-resolved skills as-is and only resolves what the Hub did not
// cover (gh://, gcp-skill://, federated registries, and refs that exist only
// in broker-local templates) through its existing router.

// maxDispatchTemplateConfigSize caps how much of a template's
// scion-agent.yaml the dispatcher will read to discover skill references.
const maxDispatchTemplateConfigSize = 1 << 20 // 1 MiB

// resolveRegistrySkillRef resolves a single scion-registry skill reference on
// behalf of identity. It is the shared core of the /skills/resolve handler and
// of dispatch-time pre-resolution.
//
// baseURL is used to rewrite local-storage file:// URLs into Hub download
// URLs. When it is empty, local-storage URLs are rewritten to Hub-relative
// paths (/api/v1/skills/...) that the broker absolutizes against its own Hub
// endpoint.
//
// ptone/scion#1901 finding F2: authorization is resolveSkill's job, not
// this function's. resolveSkill treats a candidate the caller cannot read
// exactly like one that does not exist — same "not_found" code, same
// message shape, and (unlike the pre-fix code) the scope search keeps
// looking past it instead of stopping to report a distinguishable
// "forbidden". That makes "doesn't exist" and "exists, but you can't read
// it" indistinguishable from here on, including the batch resolve surface
// that calls this once per requested URI.
func (s *Server) resolveRegistrySkillRef(
	ctx context.Context,
	identity Identity,
	rawURI string,
	uri *api.SkillURI,
	projectID, userID, baseURL string,
) (*ResolvedSkillResponse, *ResolveSkillError) {
	if uri == nil {
		return nil, &ResolveSkillError{URI: rawURI, Code: "invalid_uri", Message: "invalid skill URI"}
	}
	expandScopeAliases(uri, projectID, userID)

	skill, sv, err := s.resolveSkill(ctx, identity, uri, projectID)
	if err != nil {
		return nil, &ResolveSkillError{URI: rawURI, Code: "not_found", Message: err.Error()}
	}
	if skill == nil || sv == nil {
		return nil, &ResolveSkillError{URI: rawURI, Code: "not_found", Message: "skill not found"}
	}

	entry := &ResolvedSkillResponse{
		URI:             rawURI,
		Name:            skill.Name,
		ResolvedVersion: sv.Version,
		ContentHash:     sv.ContentHash,
	}

	if sv.Status == store.SkillVersionStatusDeprecated {
		entry.Deprecated = true
		entry.DeprecationMessage = sv.DeprecationMessage
		entry.ReplacementURI = sv.ReplacementURI
	}

	// Generate download URLs for the resolved version's files
	stor := s.GetStorage()
	if stor != nil && len(sv.Files) > 0 {
		versionPath := skill.StoragePath + "/" + sv.Version
		downloadURLs, _, _, dlErr := generateDownloadURLs(ctx, stor, versionPath, s.legacyFallbackPath(versionPath), sv.Files)
		if dlErr != nil {
			slog.ErrorContext(ctx, "failed to generate download URLs for skill version",
				"skill", skill.Name, "version", sv.Version, "error", dlErr)
			return nil, &ResolveSkillError{
				URI: rawURI, Code: "storage_error",
				Message: fmt.Sprintf("skill %s version %s has storage files missing — re-sync the skill", skill.Name, sv.Version),
			}
		}
		if stor.Provider() == storage.ProviderLocal {
			if baseURL != "" {
				downloadURLs = rewriteLocalDownloadURLs(downloadURLs, baseURL, "skills", skill.ID)
			} else {
				downloadURLs = rewriteLocalDownloadURLsRelative(downloadURLs, "skills", skill.ID)
			}
			// Pin the resolved version so the files route serves this exact
			// version (#1785), for absolute and Hub-relative URLs alike.
			downloadURLs = withSkillVersionDownloadQuery(downloadURLs, sv.Version)
			// Sign each file URL (#1792). The caller's read access was checked
			// above (or the skill is public), so the signature carries that
			// authorization to whoever downloads — typically the runtime
			// broker, which has no principal the files route would accept.
			// Each signature is bound to this skill, version and file path and
			// expires after skillFileURLTTL.
			downloadURLs = s.signSkillFileDownloadURLs(downloadURLs, skill.ID, sv.Version, time.Now())
		}
		entry.Files = downloadURLs
	}

	go func(versionID string) {
		// Best-effort counter: never let a store panic take down the Hub.
		defer func() {
			if r := recover(); r != nil {
				slog.Error("panic incrementing skill version download count",
					"version_id", versionID, "panic", r)
			}
		}()
		_ = s.store.IncrementSkillVersionDownloadCount(context.Background(), versionID)
	}(sv.ID)

	return entry, nil
}

// rewriteLocalDownloadURLsRelative is rewriteLocalDownloadURLs without a host:
// file:// URLs become Hub-relative paths. Used when the Hub resolves on the
// broker's behalf and does not know which address the broker reaches it on.
func rewriteLocalDownloadURLsRelative(urls []DownloadURLInfo, resourceType, resourceID string) []DownloadURLInfo {
	for i := range urls {
		if strings.HasPrefix(urls[i].URL, "file://") {
			urls[i].URL = "/api/v1/" + resourceType + "/" + resourceID + "/files/" + urls[i].Path + "?raw=1"
		}
	}
	return urls
}

// isHubRegistrySkillURI reports whether a skill URI is resolved from the Hub's
// own skill registry (skill://scion/... or a bare name). gh://, gcp-skill://
// and federated registries are left to the broker's resolver router.
func isHubRegistrySkillURI(raw string) (*api.SkillURI, bool) {
	if strings.HasPrefix(raw, "gh://") {
		return nil, false
	}
	uri, err := api.ParseSkillURI(raw)
	if err != nil || uri == nil {
		return nil, false
	}
	if uri.Registry != "scion" && uri.Registry != "" {
		return nil, false
	}
	return uri, true
}

// dispatchSkillRefs collects the skill references the broker will request
// for this agent that the Hub can see: the merged InlineConfig skills
// (template, hub, user, project and progeny injections) plus the skills
// declared in the Hub template's scion-agent.yaml.
func (s *Server) dispatchSkillRefs(ctx context.Context, agent *store.Agent) []api.SkillReference {
	if agent == nil || agent.AppliedConfig == nil {
		return nil
	}
	var refs []api.SkillReference
	if agent.AppliedConfig.InlineConfig != nil {
		refs = append(refs, agent.AppliedConfig.InlineConfig.Skills...)
	}
	if agent.AppliedConfig.TemplateID != "" {
		refs = append(refs, s.templateSkillRefs(ctx, agent.AppliedConfig.TemplateID)...)
	}
	return refs
}

// templateSkillRefs reads the skills declared in a Hub template's
// scion-agent.yaml (or scion-agent.json). Best-effort: any failure yields no
// refs, and the broker resolves whatever the Hub did not cover.
func (s *Server) templateSkillRefs(ctx context.Context, templateID string) []api.SkillReference {
	tmpl, err := s.store.GetTemplate(ctx, templateID)
	if err != nil || tmpl == nil {
		return nil
	}
	stor := s.GetStorage()
	if stor == nil || tmpl.StoragePath == "" {
		return nil
	}
	for _, name := range []string{"scion-agent.yaml", "scion-agent.yml", "scion-agent.json"} {
		found := false
		for _, f := range tmpl.Files {
			if f.Path == name {
				found = true
				break
			}
		}
		if !found {
			continue
		}
		data, err := readDispatchTemplateFile(ctx, stor, tmpl.StoragePath+"/"+name)
		if err != nil {
			if !errors.Is(err, storage.ErrNotFound) {
				slog.WarnContext(ctx, "dispatch skill pre-resolution: failed to read template config",
					"template_id", templateID, "file", name, "error", err)
			}
			return nil
		}
		cfg, err := config.ParseScionAgentConfig(name, data)
		if err != nil {
			slog.WarnContext(ctx, "dispatch skill pre-resolution: failed to parse template config",
				"template_id", templateID, "file", name, "error", err)
			return nil
		}
		return cfg.Skills
	}
	return nil
}

// readDispatchTemplateFile downloads a template file, reading at most
// maxDispatchTemplateConfigSize bytes.
func readDispatchTemplateFile(ctx context.Context, stor storage.Storage, path string) ([]byte, error) {
	reader, _, err := stor.Download(ctx, path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()
	return io.ReadAll(io.LimitReader(reader, maxDispatchTemplateConfigSize))
}

// skillResolveIdentityForAgent returns the principal whose permissions govern
// skill resolution for an agent being dispatched: the authenticated caller
// driving the dispatch when there is one (the creating user, or the parent
// agent creating a child), otherwise the agent's recorded creator. Broker and
// other non-user/agent identities never qualify.
func (s *Server) skillResolveIdentityForAgent(ctx context.Context, agent *store.Agent) Identity {
	switch id := GetIdentityFromContext(ctx).(type) {
	case UserIdentity:
		return id
	case AgentIdentity:
		return id
	}
	return s.creatorIdentityForAgent(ctx, agent)
}

// creatorIdentityForAgent reconstructs the identity of the principal that
// created agent (agent.CreatedBy). The creator kind is not stored, so an agent
// lookup is tried first and a user lookup second (mirrors the scheduled-message
// creator resolution). Returns nil when the creator cannot be resolved or is
// no longer active.
func (s *Server) creatorIdentityForAgent(ctx context.Context, agent *store.Agent) Identity {
	if agent == nil || agent.CreatedBy == "" {
		return nil
	}
	if creator, err := s.store.GetAgent(ctx, agent.CreatedBy); err == nil {
		if creator == nil || !creator.DeletedAt.IsZero() {
			return nil
		}
		role, additionalScopes := agentRoleAndScopes(creator)
		scopes := append(ScopesForRole(role), additionalScopes...)
		return &agentIdentityWrapper{&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: creator.ID},
			ProjectID: creator.ProjectID,
			Scopes:    scopes,
			Ancestry:  creator.Ancestry,
		}}
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil
	}
	user, err := s.store.GetUser(ctx, agent.CreatedBy)
	if err != nil || user == nil || user.Status != store.UserStatusActive {
		return nil
	}
	return NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "dispatch")
}

// preResolveAgentSkills resolves the agent's Hub-registry skill references as
// the agent's creator (see skillResolveIdentityForAgent). Per-skill failures,
// including authorization denials, are reported in Errors and are
// authoritative: the broker does not retry them with its own identity.
// Returns nil when there is nothing to pre-resolve or no principal is
// available, in which case the broker resolves as before.
func (s *Server) preResolveAgentSkills(ctx context.Context, agent *store.Agent) *ResolveSkillsResponse {
	return s.preResolveAgentSkillsAsIdentity(ctx, agent, s.skillResolveIdentityForAgent(ctx, agent))
}

// preResolveAgentSkillsAsCreator resolves the agent's Hub-registry skill
// references as the agent's recorded creator (agent.CreatedBy), regardless of
// which permitted principal is driving the current dispatch. Start and
// restart use this so a given agent's pre-resolved set is the same whoever
// starts or restarts it — the creator, an admin, or a project owner all get
// the same, creator-based resolution (ptone/scion#1994). A parent-agent
// creator resolves through the same creatorIdentityForAgent path used
// elsewhere.
//
// When agent.CreatedBy is empty (no recorded creator) or the recorded
// creator no longer exists, pre-resolution is skipped: this never falls back
// to the dispatching caller's identity or to agent.OwnerID. A skill that
// needed pre-resolution then goes unresolved here, and any required skill
// reaches the broker's own resolution attempt, which reports the same clean,
// existing "could not be resolved" outcome it already reports for other
// unresolvable skills — no panic, no new error path.
func (s *Server) preResolveAgentSkillsAsCreator(ctx context.Context, agent *store.Agent) *ResolveSkillsResponse {
	return s.preResolveAgentSkillsAsIdentity(ctx, agent, s.creatorIdentityForAgent(ctx, agent))
}

// preResolveAgentSkillsAsIdentity is the shared core of preResolveAgentSkills
// and preResolveAgentSkillsAsCreator: it resolves the agent's dispatchable
// Hub-registry skill references as identity, which the two callers derive
// differently.
func (s *Server) preResolveAgentSkillsAsIdentity(ctx context.Context, agent *store.Agent, identity Identity) *ResolveSkillsResponse {
	refs := s.dispatchSkillRefs(ctx, agent)
	if len(refs) == 0 {
		return nil
	}

	type pending struct {
		raw string
		uri *api.SkillURI
	}
	seen := make(map[string]bool, len(refs))
	var todo []pending
	for _, ref := range refs {
		if ref.URI == "" || seen[ref.URI] {
			continue
		}
		uri, ok := isHubRegistrySkillURI(ref.URI)
		if !ok {
			continue
		}
		seen[ref.URI] = true
		todo = append(todo, pending{raw: ref.URI, uri: uri})
	}
	if len(todo) == 0 {
		return nil
	}

	if identity == nil {
		slog.WarnContext(ctx, "dispatch skill pre-resolution skipped: no creator identity available",
			"agent_id", agent.ID, "created_by", agent.CreatedBy)
		return nil
	}

	resp := &ResolveSkillsResponse{}
	aliasUserID := dispatchSkillAliasUserID(agent)
	for _, p := range todo {
		entry, resolveErr := s.resolveRegistrySkillRef(ctx, identity, p.raw, p.uri,
			agent.ProjectID, aliasUserID, "")
		if resolveErr != nil {
			resp.Errors = append(resp.Errors, *resolveErr)
			continue
		}
		resp.Resolved = append(resp.Resolved, *entry)
	}
	return resp
}

// dispatchSkillAliasUserID returns the user ID used to expand a bare
// skill://user alias (no explicit scope ID) at dispatch time: the agent's
// origin user, Ancestry[0], the root human at the head of the creation
// chain. For a human-created agent this is the same value as
// OwnerID/CreatedBy (its Ancestry is exactly [userID]); for an agent-created
// child it is the chain's root user, not the immediate parent agent that
// OwnerID/CreatedBy record.
//
// This deliberately does not fall back to CreatedBy the way
// resolveOriginUserID (authorize_message.go) does: an agent with no recorded
// Ancestry gets no user-scope alias at all here, rather than one keyed off
// the wrong principal.
func dispatchSkillAliasUserID(agent *store.Agent) string {
	if agent == nil || len(agent.Ancestry) == 0 {
		return ""
	}
	return agent.Ancestry[0]
}
