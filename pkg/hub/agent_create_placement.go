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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Placement for agent-launched creates.
//
// When an agent creates another agent and the request does not name a
// broker or profile, the hub fills them in before any other create check
// runs. Precedence, per field, highest first:
//
//  1. the request (the CLI's --broker / -p flags);
//  2. the project's agent-create settings (scion.io/agent-create-broker and
//     scion.io/agent-create-profile);
//  3. the creating agent's own broker and profile, when the
//     hub.agent_create_inherit_placement experiment is on, the creator runs
//     on a Kubernetes profile and its broker serves the target project.
//     "Kubernetes" is isKubernetesRuntimeType on the profile type the broker
//     registered, which today is the profile's runtime key, not the runtime
//     entry's resolved type (see the comment on isKubernetesRuntimeType);
//  4. the regular default chain, unchanged (project default broker and
//     active profile, hub defaults, broker default, automatic selection).
//
// A profile is inherited only when the broker that ends up selected is the
// creator's broker, so an explicit --broker elsewhere never carries the
// creator's profile with it. With -p alone the creator's broker is still
// inherited, and the create is refused when that broker does not offer the
// requested profile (the error names the profile and the broker). A profile
// from the agent-create profile setting that the creator's broker does not
// offer does not inherit the broker; the regular chain picks one, and the
// setting check below applies to it.
//
// Creates by users never take tiers 2 and 3.
//
// The filled-in values are written into the request exactly as if the caller
// had passed them, so every later check (broker dispatch authorization, the
// per-profile default identity, env and secret resolution, network policy)
// evaluates them under the caller's own authority. Nothing else is taken
// from the creator: no credentials, identity or user scope.
//
// Fail closed: tier 3 applies only when it can be confirmed (creator found,
// broker serves the project, profile resolves to a Kubernetes type);
// otherwise it is not attempted and tier 4 runs. Once tier 2 or 3 applies,
// a value that does not resolve or that the caller may not use refuses the
// create instead of falling back. Error messages name the source, never IDs.

// agentCreatePlacement records how the broker and profile of a create were
// chosen. Sources are store.PlacementSource* values; an empty source means
// the regular default chain decides it.
type agentCreatePlacement struct {
	brokerSource  string
	profileSource string
	// brokerName is the broker's name when tier 2 or 3 chose it, for audit.
	brokerName string
	// profile is the profile chosen by tier 2 or 3, for audit.
	profile string
}

// record returns the placement record stored on the agent, with the default
// source filled in for any field the request and tiers 2-3 left open.
func (p *agentCreatePlacement) record() *store.AgentPlacement {
	rec := &store.AgentPlacement{BrokerSource: p.brokerSource, ProfileSource: p.profileSource}
	if rec.BrokerSource == "" {
		rec.BrokerSource = store.PlacementSourceDefault
	}
	if rec.ProfileSource == "" {
		rec.ProfileSource = store.PlacementSourceDefault
	}
	return rec
}

// auditSummary returns a compact JSON summary of a placement chosen by a
// setting or by inheritance, naming the broker and profile by name only.
// It returns "" when neither field came from tier 2 or 3.
func (p *agentCreatePlacement) auditSummary() string {
	derived := func(src string) bool {
		return src == store.PlacementSourceSetting || src == store.PlacementSourceInherited
	}
	if !derived(p.brokerSource) && !derived(p.profileSource) {
		return ""
	}
	out := map[string]string{
		"brokerSource":  p.record().BrokerSource,
		"profileSource": p.record().ProfileSource,
	}
	if derived(p.brokerSource) && p.brokerName != "" {
		out["broker"] = p.brokerName
	}
	if derived(p.profileSource) && p.profile != "" {
		out["profile"] = p.profile
	}
	b, err := json.Marshal(map[string]any{"placement": out})
	if err != nil {
		return ""
	}
	return string(b)
}

// placementSourceDescription is the user-facing name of a tier-2/3 source,
// used in error messages instead of any ID.
func placementSourceDescription(source, field string) string {
	if source == store.PlacementSourceSetting {
		return "the project's agent-create " + field + " setting"
	}
	return "the creating agent's " + field
}

// applyAgentCreatePlacement fills req.RuntimeBrokerID and req.Profile for an
// agent-launched create from tiers 2 and 3 (see the comment at the top of
// this file). It writes an error response and returns ok=false when an
// applicable value cannot be used.
func (s *Server) applyAgentCreatePlacement(ctx context.Context, w http.ResponseWriter, project *store.Project, req *CreateAgentRequest) (*agentCreatePlacement, bool) {
	p := &agentCreatePlacement{}
	explicitBroker := req.RuntimeBrokerID != ""
	if explicitBroker {
		p.brokerSource = store.PlacementSourceFlag
	}
	if req.Profile != "" {
		p.profileSource = store.PlacementSourceFlag
	}

	agentIdent := GetAgentIdentityFromContext(ctx)
	if agentIdent == nil || project == nil {
		return p, true
	}

	// Tier 2: the project's agent-create settings.
	settings := projectSettingsFromAnnotations(project)
	var selected *store.RuntimeBroker
	if !explicitBroker && settings.AgentCreateBroker != nil {
		broker, ok := s.placementBroker(ctx, w, project, *settings.AgentCreateBroker, store.PlacementSourceSetting)
		if !ok {
			return nil, false
		}
		selected = broker
		req.RuntimeBrokerID = broker.ID
		p.brokerSource = store.PlacementSourceSetting
		p.brokerName = broker.Name
	}
	if req.Profile == "" && !explicitBroker && settings.AgentCreateProfile != nil {
		req.Profile = *settings.AgentCreateProfile
		p.profileSource = store.PlacementSourceSetting
		p.profile = req.Profile
	}

	// Tier 3: inherit the creator's placement.
	if (req.RuntimeBrokerID == "" || req.Profile == "") && s.experimentEnabled(experiments.AgentCreateInheritPlacement) {
		creatorBroker, creatorProfile, applicable, ok := s.creatorKubernetesPlacement(ctx, w, project, agentIdent.ID())
		if !ok {
			return nil, false
		}
		if applicable {
			// -p alone inherits the creator's broker; a profile that broker
			// does not offer refuses the create instead of moving it to
			// another broker.
			if req.RuntimeBrokerID == "" && p.profileSource == store.PlacementSourceFlag && !brokerOffersProfile(creatorBroker, req.Profile) {
				writeError(w, http.StatusUnprocessableEntity, ErrCodeNoRuntimeBroker,
					fmt.Sprintf("The creating agent's broker %q does not offer profile %q; pass --broker to choose a broker that offers it, or pass another profile", creatorBroker.Name, req.Profile),
					map[string]interface{}{"source": store.PlacementSourceInherited})
				return nil, false
			}
			if req.RuntimeBrokerID == "" && (req.Profile == "" || brokerOffersProfile(creatorBroker, req.Profile)) {
				broker, ok := s.placementBroker(ctx, w, project, creatorBroker.ID, store.PlacementSourceInherited)
				if !ok {
					return nil, false
				}
				selected = broker
				req.RuntimeBrokerID = broker.ID
				p.brokerSource = store.PlacementSourceInherited
				p.brokerName = broker.Name
			}
			if req.Profile == "" && brokerMatches(creatorBroker, req.RuntimeBrokerID) {
				req.Profile = creatorProfile
				p.profileSource = store.PlacementSourceInherited
				p.profile = creatorProfile
			}
		}
	}

	// A tier-2 profile must be offered by the broker it will run on. When
	// tiers 2-3 chose the broker it is checked here; otherwise the regular
	// chain picks the broker later and checkAgentCreateProfileSetting runs.
	if p.profileSource == store.PlacementSourceSetting && selected != nil && !brokerOffersProfile(selected, req.Profile) {
		writeAgentCreateProfileNotOffered(w)
		return nil, false
	}

	if p.brokerSource == store.PlacementSourceSetting || p.brokerSource == store.PlacementSourceInherited ||
		p.profileSource == store.PlacementSourceSetting || p.profileSource == store.PlacementSourceInherited {
		slog.Info("Agent-launched create placement",
			"project_id", project.ID, "parent_agent_id", agentIdent.ID(),
			"broker", p.brokerName, "broker_source", p.record().BrokerSource,
			"profile", p.profile, "profile_source", p.record().ProfileSource)
	}
	return p, true
}

// checkAgentCreateProfileSetting refuses a create whose profile came from the
// project's agent-create profile setting when the broker the regular chain
// selected does not offer it. It writes the error response and returns false.
func (s *Server) checkAgentCreateProfileSetting(ctx context.Context, w http.ResponseWriter, p *agentCreatePlacement, brokerID, profile string) bool {
	if p == nil || p.profileSource != store.PlacementSourceSetting || brokerID == "" {
		return true
	}
	broker, err := s.store.GetRuntimeBroker(ctx, brokerID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAgentCreateProfileNotOffered(w)
			return false
		}
		writeErrorFromErr(w, err, "")
		return false
	}
	if !brokerOffersProfile(broker, profile) {
		writeAgentCreateProfileNotOffered(w)
		return false
	}
	return true
}

func writeAgentCreateProfileNotOffered(w http.ResponseWriter) {
	writeError(w, http.StatusUnprocessableEntity, ErrCodeNoRuntimeBroker,
		"The project's agent-create profile setting names a profile the selected broker does not offer; pass -p or --broker, or update the setting",
		map[string]interface{}{"source": store.PlacementSourceSetting})
}

// placementBroker resolves a broker chosen by tier 2 or 3 and checks it the
// same way an explicit --broker would be checked for the caller: it must
// serve the project, be online, and be usable by the caller. ref is a broker
// ID (or, for a setting written before normalization, a name or slug).
func (s *Server) placementBroker(ctx context.Context, w http.ResponseWriter, project *store.Project, ref, source string) (*store.RuntimeBroker, bool) {
	what := placementSourceDescription(source, "broker")
	details := map[string]interface{}{"source": source}
	broker, err := s.projectProviderBroker(ctx, project.ID, ref)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusUnprocessableEntity, ErrCodeNoRuntimeBroker,
				"No broker serving this project matches "+what+"; pass --broker, or update the setting", details)
			return nil, false
		}
		writeErrorFromErr(w, err, "")
		return nil, false
	}
	if !s.brokerRecordReachable(broker) {
		writeError(w, http.StatusServiceUnavailable, ErrCodeRuntimeBrokerUnavail,
			"The broker named by "+what+" is offline; pass --broker to choose another", details)
		return nil, false
	}
	if !s.canUseBrokerForProject(ctx, broker, project) {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"You don't have permission to create agents on the broker named by "+what, details)
		return nil, false
	}
	return broker, true
}

// projectProviderBroker returns the broker record of a provider of
// projectID matching ref by ID, or by name or slug case-insensitively. It
// returns store.ErrNotFound when no provider matches or its broker record
// no longer exists.
func (s *Server) projectProviderBroker(ctx context.Context, projectID, ref string) (*store.RuntimeBroker, error) {
	if ref == "" {
		return nil, store.ErrNotFound
	}
	providers, err := s.store.GetProjectProviders(ctx, projectID)
	if err != nil {
		return nil, err
	}
	for _, pv := range providers {
		if pv.BrokerID == ref {
			return s.store.GetRuntimeBroker(ctx, pv.BrokerID)
		}
	}
	for _, pv := range providers {
		broker, err := s.store.GetRuntimeBroker(ctx, pv.BrokerID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return nil, err
		}
		if strings.EqualFold(broker.Name, ref) || strings.EqualFold(broker.Slug, ref) {
			return broker, nil
		}
	}
	return nil, store.ErrNotFound
}

// creatorKubernetesPlacement reports the creating agent's broker and
// resolved profile when inheritance applies: the creator exists, its broker
// serves the target project and still exists, and its profile resolves on
// that broker to a Kubernetes runtime type. applicable=false means
// inheritance is not attempted. ok=false means an error response was written.
func (s *Server) creatorKubernetesPlacement(ctx context.Context, w http.ResponseWriter, project *store.Project, creatorID string) (broker *store.RuntimeBroker, profile string, applicable, ok bool) {
	creator, err := s.store.GetAgent(ctx, creatorID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, "", false, true
		}
		writeErrorFromErr(w, err, "")
		return nil, "", false, false
	}
	if creator.RuntimeBrokerID == "" {
		return nil, "", false, true
	}
	if _, err := s.store.GetProjectProvider(ctx, project.ID, creator.RuntimeBrokerID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, "", false, true
		}
		writeErrorFromErr(w, err, "")
		return nil, "", false, false
	}
	broker, err = s.store.GetRuntimeBroker(ctx, creator.RuntimeBrokerID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, "", false, true
		}
		writeErrorFromErr(w, err, "")
		return nil, "", false, false
	}
	saved := ""
	if creator.AppliedConfig != nil {
		saved = creator.AppliedConfig.Profile
	}
	resolved, runtimeType, found := resolveAgentRuntimeProfileType(broker, saved)
	if !found || !isKubernetesRuntimeType(runtimeType) {
		return nil, "", false, true
	}
	return broker, resolved, true, true
}

// brokerOffersProfile reports whether broker registers a profile named
// profile and reports it available (BrokerProfile.Available, the same test
// reincarnate's brokerProfileAvailable applies). A broker that registers no
// profiles cannot confirm it, so false. The settings PUT uses the same test,
// so a setting cannot name a profile the broker reports unavailable; a
// profile that becomes unavailable later refuses the create at create time.
func brokerOffersProfile(broker *store.RuntimeBroker, profile string) bool {
	if broker == nil || profile == "" {
		return false
	}
	for _, bp := range broker.Profiles {
		if bp.Name == profile {
			return bp.Available
		}
	}
	return false
}

// brokerMatches reports whether ref names broker by ID, name or slug.
func brokerMatches(broker *store.RuntimeBroker, ref string) bool {
	if broker == nil || ref == "" {
		return false
	}
	return ref == broker.ID || strings.EqualFold(ref, broker.Name) || strings.EqualFold(ref, broker.Slug)
}

// validateAgentCreatePlacementSettings checks the agent-create placement
// settings on a project settings PUT. It runs only when the request carries
// either field, so an unrelated save is never refused because a broker went
// away later. The broker must be a provider of the project and is stored as
// its ID; the profile must be offered by that broker or, with no broker set,
// by at least one of the project's brokers. It writes the error response and
// returns false when the caller must stop.
func (s *Server) validateAgentCreatePlacementSettings(w http.ResponseWriter, ctx context.Context, project *store.Project, req *hubclient.ProjectSettings) bool {
	if req.AgentCreateBroker == nil && req.AgentCreateProfile == nil {
		return true
	}

	brokerRef := project.Annotations[projectSettingAgentCreateBroker]
	if req.AgentCreateBroker != nil {
		brokerRef = strings.TrimSpace(*req.AgentCreateBroker)
	}
	profile := project.Annotations[projectSettingAgentCreateProfile]
	if req.AgentCreateProfile != nil {
		profile = strings.TrimSpace(*req.AgentCreateProfile)
		if profile != "" && !profileDefaultNamePattern.MatchString(profile) {
			BadRequest(w, fmt.Sprintf("agentCreateProfile: profile name %q is invalid; use 1-%d letters, digits, '-' or '_', starting with a letter or digit", profile, maxProfileDefaultNameLen))
			return false
		}
		req.AgentCreateProfile = &profile
	}

	var broker *store.RuntimeBroker
	if brokerRef != "" {
		b, err := s.projectProviderBroker(ctx, project.ID, brokerRef)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				BadRequest(w, "agentCreateBroker: no broker serving this project has that ID, name or slug")
				return false
			}
			writeErrorFromErr(w, err, "")
			return false
		}
		broker = b
		if req.AgentCreateBroker != nil {
			id := b.ID
			req.AgentCreateBroker = &id
		}
	} else if req.AgentCreateBroker != nil {
		empty := ""
		req.AgentCreateBroker = &empty
	}

	if profile == "" {
		return true
	}
	if broker != nil {
		if !brokerOffersProfile(broker, profile) {
			BadRequest(w, fmt.Sprintf("agentCreateProfile: the agent-create broker does not offer profile %q", profile))
			return false
		}
		return true
	}
	providers, err := s.store.GetProjectProviders(ctx, project.ID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return false
	}
	for _, pv := range providers {
		b, err := s.store.GetRuntimeBroker(ctx, pv.BrokerID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			writeErrorFromErr(w, err, "")
			return false
		}
		if brokerOffersProfile(b, profile) {
			return true
		}
	}
	BadRequest(w, fmt.Sprintf("agentCreateProfile: no broker serving this project offers profile %q", profile))
	return false
}
