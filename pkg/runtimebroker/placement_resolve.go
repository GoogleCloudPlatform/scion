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
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Kubernetes placement defaults (ptone/scion#5085): the node selector,
// tolerations and runtime class an agent pod gets from the agent runtime's
// global settings. They follow the runtime the pod runs on: configured
// global Kubernetes runtime entries (config.ConfiguredRuntimeNames) that
// target that runtime's cluster and
// namespace (matched by identity, not by name) give the entry rung, and a
// global profile gives the profile rung only when the runtime choice used
// it. Whatever cannot be determined fails the start with a fixed cause; the
// detail goes to the agent runtime log. Resolved on every leg that starts a
// pod, before that leg's side effects, and never persisted.

// Fixed causes returned to the client. They name no settings path and carry
// no parser output.
const (
	placementGlobalUnreadableCause = "the agent runtime's global settings could not be read; Kubernetes placement cannot be determined"
	placementLegacyGlobalCause     = "the agent runtime's global settings use the legacy format and mention Kubernetes placement keys; set schema_version: \"1\" so Kubernetes placement can be determined"
	placementBuiltFromCause        = "the runtime entry this agent's runtime was built from no longer matches its cluster and namespace; restart the agent runtime, fix the entry, or align the settings file's entry with the stored one"
	placementAgentConfigCause      = "the agent's config could not be read to check it against the agent runtime's Kubernetes placement"
)

// placementDefaultsOnlyCause is the fixed cause for a profile with placement
// whose runtime entry exists only in the embedded default settings.
func placementDefaultsOnlyCause(profile, entry string) string {
	return fmt.Sprintf("profile %s sets Kubernetes placement, but its runtime entry %s is defined only by the built-in defaults; define runtimes.%s in the agent runtime's settings so its placement can be matched", profile, entry, entry)
}

func placementUndeterminedCause(entry, cause string) string {
	return fmt.Sprintf("cannot determine whether runtime entry %s targets this agent's runtime (%s)", entry, cause)
}

// placementRefusal is the error a placement refusal returns: a 400
// validation error with the fixed cause as its message. The hub relays an
// agent runtime's 400 validation_error with its message intact, does not retry
// it, and treats it as a confirmed refusal of the start.
func placementRefusal(cause string) error {
	return &startContextError{Status: http.StatusBadRequest, Code: ErrCodeValidationError, Message: cause}
}

// forCreate settles How for a create: a profile resolution with an empty
// requested profile used the project's active profile, which is a create's
// selection profile, so it counts as a profile resolution.
func (r runtimeResolution) forCreate(agentName string) runtimeResolution {
	r.Agent = agentName
	if r.How == resolutionUnset && r.Profile != "" {
		r.How = resolutionProfile
	}
	return r
}

// forExistingAgent settles How for a start or restart of an existing
// agent: a profile resolution with an empty selection profile is an agent
// with no recorded profile.
func (r runtimeResolution) forExistingAgent(agentName string) runtimeResolution {
	r.Agent = agentName
	if r.How == resolutionUnset {
		r.How = resolutionUnrecorded
	}
	return r
}

// placementForStart is resolvePlacement with the project's settings (the
// project-merged view, for the ignored-values warning) loaded from
// projectPath; a load failure leaves them out.
func (s *Server) placementForStart(ctx context.Context, res runtimeResolution, projectPath string) (*api.ResolvedPlacement, error) {
	var projectVS *config.VersionedSettings
	if res.Runtime != nil && res.Runtime.Name() == "kubernetes" && res.How != resolutionFlat && !s.isFlat() {
		// Without a resolved project directory the loader would fall back to
		// the working directory's project; skip the warning input instead.
		if projectDir, err := config.GetResolvedProjectDir(projectPath); err == nil {
			if vs, _, err := s.loadRuntimeSettings(projectDir); err == nil {
				projectVS = vs
			}
		}
	}
	return s.resolvePlacement(ctx, res, projectVS)
}

// resolvePlacement resolves the Kubernetes placement for one start on the
// runtime res names. It returns nil when no placement applies (not a
// Kubernetes runtime, a flat instance, a runtime that failed to build, or
// nothing configured for this runtime), and a placementRefusal when the
// placement cannot be determined.
func (s *Server) resolvePlacement(ctx context.Context, res runtimeResolution, projectVS *config.VersionedSettings) (*api.ResolvedPlacement, error) {
	if res.How == resolutionErrorRuntime {
		// A runtime that failed to build starts nothing.
		return nil, nil
	}
	if s.isFlat() || res.How == resolutionFlat {
		// Every start on a flat instance, including an existing agent's
		// start or restart on the runtime its container was found on,
		// takes the instance's own placement.
		return s.resolveFlatPlacement(res.Runtime, res.Agent)
	}
	if res.Runtime == nil || res.Runtime.Name() != "kubernetes" {
		s.warnPlacementOffKubernetes(ctx, res)
		return nil, nil
	}

	// Step 2: the global settings, failing closed.
	gvs, _, err := config.LoadGlobalSettingsWithOverlayFor(ctx)
	if err != nil {
		s.agentLifecycleLog.Warn("Kubernetes placement: global settings could not be read", "agent", res.Agent, "error", err)
		return nil, placementRefusal(placementGlobalUnreadableCause)
	}
	// The loader treats a global settings file it cannot stat as missing, so
	// the settings above may lack the operator's file: fail closed.
	if config.GlobalSettingsFileUnstatable() {
		s.agentLifecycleLog.Warn("Kubernetes placement: the global settings file cannot be stat'd", "agent", res.Agent)
		return nil, placementRefusal(placementGlobalUnreadableCause)
	}
	if config.GlobalSettingsIsLegacyFormat() && config.GlobalSettingsMentionPlacementKeys() {
		s.agentLifecycleLog.Warn("Kubernetes placement: legacy-format global settings mention a placement key", "agent", res.Agent)
		return nil, placementRefusal(placementLegacyGlobalCause)
	}
	if gvs == nil {
		gvs = &config.VersionedSettings{}
	}
	s.warnProjectPlacement(res.Agent, gvs, projectVS)

	// Step 3: the entry rung, by identity.
	rctx, rns, hasClient, conn := kubernetesPlacementIdentity(res.Runtime)
	defaultNS := scionrt.DefaultKubernetesNamespace()
	live := memoLiveCurrentContext()

	// Only configured runtime entries take part: an entry that exists only
	// in the embedded default settings carries no placement and is not the
	// operator's. When that cannot be told, every entry takes part.
	configured, configuredKnown := config.ConfiguredRuntimeNames()
	if !configuredKnown {
		s.agentLifecycleLog.Warn("Kubernetes placement: cannot tell configured runtime entries from defaults; considering all of them", "agent", res.Agent)
	}
	isConfigured := func(name string) bool { return !configuredKnown || configured[name] }

	names := make([]string, 0, len(gvs.Runtimes))
	for name, rt := range gvs.Runtimes {
		if config.IsKubernetesRuntimeEntry(name, rt) && isConfigured(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	profilesOn := func(entries map[string]bool) []string {
		var out []string
		for name, p := range gvs.Profiles {
			if entries[p.Runtime] && config.ProfileHasPlacement(p) {
				out = append(out, name)
			}
		}
		sort.Strings(out)
		return out
	}
	carries := func(entry string) bool {
		return config.RuntimeEntryHasPlacement(gvs.Runtimes[entry]) || len(profilesOn(map[string]bool{entry: true})) > 0
	}

	var matched []string
	inE := map[string]bool{}
	for _, name := range names {
		result, cause := config.MatchKubernetesEntry(gvs.Runtimes[name], rctx, rns, hasClient, conn, res.Record, defaultNS, live)
		switch result {
		case config.Match:
			matched = append(matched, name)
			inE[name] = true
		case config.Undetermined:
			if carries(name) {
				s.agentLifecycleLog.Warn("Kubernetes placement: cannot match a runtime entry that carries placement",
					"agent", res.Agent, "entry", name, "cause", cause, "context", rctx, "namespace", rns, "connection", conn.String())
				return nil, placementRefusal(placementUndeterminedCause(name, cause))
			}
		}
	}

	// Entries defined only by the embedded defaults stay out of the entry
	// rung and sibling agreement. One that would match this runtime still
	// matters for the profile rung: a global profile with placement on it
	// cannot apply, and is refused with its own cause below.
	defaultsOnlyMatched := map[string]bool{}
	if configuredKnown {
		for name, rt := range gvs.Runtimes {
			if configured[name] || !config.IsKubernetesRuntimeEntry(name, rt) {
				continue
			}
			if result, _ := config.MatchKubernetesEntry(rt, rctx, rns, hasClient, conn, res.Record, defaultNS, live); result == config.Match {
				defaultsOnlyMatched[name] = true
			}
		}
	}

	anyMatchedCarries := false
	for _, name := range matched {
		if carries(name) {
			anyMatchedCarries = true
		}
	}
	for _, builtFrom := range []string{res.Record.EntryName, res.DispatchEntry} {
		if builtFrom == "" || inE[builtFrom] {
			continue
		}
		rt, ok := gvs.Runtimes[builtFrom]
		if !ok || !config.IsKubernetesRuntimeEntry(builtFrom, rt) || !isConfigured(builtFrom) {
			continue
		}
		if carries(builtFrom) || anyMatchedCarries {
			s.agentLifecycleLog.Warn("Kubernetes placement: the runtime's entry no longer matches it",
				"agent", res.Agent, "entry", builtFrom, "recordEntry", res.Record.EntryName, "dispatchEntry", res.DispatchEntry,
				"context", rctx, "namespace", rns, "matched", matched)
			return nil, placementRefusal(placementBuiltFromCause)
		}
	}

	for _, name := range matched[min(1, len(matched)):] {
		if !config.SiblingPlacementAgrees(gvs.Runtimes[matched[0]], gvs.Runtimes[name]) {
			s.agentLifecycleLog.Warn("Kubernetes placement: identity-matched runtime entries disagree",
				"agent", res.Agent, "entries", []string{matched[0], name})
			return nil, placementRefusal(config.SiblingDisagreementCause(matched[0], name))
		}
	}

	rp := config.LegacyPlacementPolicy(gvs.Runtimes, matched, "", nil)

	// Step 4: the profile rung.
	pp := profilesOn(inE)
	profile, profileGlobal := gvs.Profiles[res.Profile]
	profileGlobal = profileGlobal && res.Profile != ""
	switch {
	case res.How == resolutionProfile && profileGlobal && inE[profile.Runtime]:
		config.ApplyProfilePlacement(rp, res.Profile, profile)
	case res.How == resolutionProfile && profileGlobal && defaultsOnlyMatched[profile.Runtime]:
		// The selected global profile's runtime entry exists only in the
		// built-in defaults and matches this runtime. Like the case above, it
		// settles the profile rung; its placement cannot be matched, though,
		// so a profile that sets placement is refused. Without placement it
		// is the entry rung only, as above with a profile that sets none.
		if config.ProfileHasPlacement(profile) {
			s.agentLifecycleLog.Warn("Kubernetes placement: the selected profile's runtime entry is defined only by the built-in defaults",
				"agent", res.Agent, "profile", res.Profile, "entry", profile.Runtime)
			return nil, placementRefusal(placementDefaultsOnlyCause(res.Profile, profile.Runtime))
		}
	case res.How == resolutionForced:
		// The forced runtime takes the entry rung only.
	default:
		profileElsewhere := profileGlobal && config.ProfileHasPlacement(profile) && !inE[profile.Runtime]
		if len(pp) > 0 || profileElsewhere {
			// The cause lists every placement profile that targets this
			// runtime, and names the entry to move placement onto, both over
			// the configured and the defaults-only matching entries (the
			// first entry name in sorted order), so the text does not depend
			// on whether such an entry is configured.
			listed := profilesOn(unionSet(inE, defaultsOnlyMatched))
			cause := s.profileRungCause(res, profileGlobal, sortedNames(unionSet(inE, defaultsOnlyMatched)), listed)
			s.agentLifecycleLog.Warn("Kubernetes placement: the profile rung cannot be determined",
				"agent", res.Agent, "how", res.How.String(), "profile", res.Profile, "profiles", listed, "matched", matched)
			return nil, placementRefusal(cause)
		}
		if ppD := profilesOn(defaultsOnlyMatched); len(ppD) > 0 {
			s.agentLifecycleLog.Warn("Kubernetes placement: a profile with placement uses a runtime entry defined only by the built-in defaults",
				"agent", res.Agent, "how", res.How.String(), "profiles", ppD)
			return nil, placementRefusal(placementDefaultsOnlyCause(ppD[0], gvs.Profiles[ppD[0]].Runtime))
		}
	}

	if !rp.InEffect() {
		return nil, nil
	}
	s.agentLifecycleLog.Debug("Kubernetes placement resolved", "agent", res.Agent,
		"nodeSelector", rp.NodeSelector, "tolerations", rp.Tolerations, "runtimeClassName", rp.RuntimeClassName, "sources", rp.Sources)
	return rp, nil
}

// Fixed causes of a flat instance's placement refusals.
const (
	placementFlatUnresolvedCause = "runtime placement is unresolved; reload the broker configuration"
	placementFlatInvalidCause    = "the instance's runtime_target placement is not valid; fix it in the instance's settings and restart the agent runtime"
	placementFlatRuntimeCause    = "the agent's runtime is not this instance's runtime target; restart the agent runtime, or re-create the agent on this instance"
	// placementFlatRestartNoProjectCause refuses a restart on a flat
	// instance whose project path is empty after every recovery (the
	// request's path or slug, the matched container's recorded path).
	placementFlatRestartNoProjectCause = "cannot check the agent's stored config before restart: no project path; restart the agent from its project"
)

// resolveFlatPlacement resolves the placement of a start on this flat
// instance: the node selector, tolerations and runtime class of the
// instance's runtime_target, from the policy activated when the instance
// started (never the legacy runtime entries or profiles). The result is
// operator-owned (api.ResolvedPlacement.OperatorOwned), even when the
// target sets none of the fields, so the agent's own fields are always
// checked against it. Nil for a target other than Kubernetes, which cannot
// set placement. rt is the runtime the start resolved (nil before one is
// resolved, as for a create's early check); for a Kubernetes target, a
// runtime other than the instance's own refuses the start rather than
// placing the pod by this target or starting it without placement. The target is validated again here; an invalid one refuses the
// start.
func (s *Server) resolveFlatPlacement(rt scionrt.Runtime, agentName string) (*api.ResolvedPlacement, error) {
	p := s.flatInstancePolicy()
	if p == nil {
		s.agentLifecycleLog.Warn("Kubernetes placement: the flat instance has no activated policy", "agent", agentName)
		return nil, placementRefusal(placementFlatUnresolvedCause)
	}
	t := p.Target
	if t.Type != config.RuntimeTargetTypeKubernetes {
		return nil, nil
	}
	if rt != nil {
		if _, own, _ := s.defaultPair(); rt != own {
			s.agentLifecycleLog.Warn("Kubernetes placement: the start resolved a runtime other than the instance's own", "agent", agentName, "runtime", rt.Name())
			return nil, placementRefusal(placementFlatRuntimeCause)
		}
	}
	source := "server.broker.instances[" + p.InstanceKey + "].runtime_target"
	if errs := config.ValidatePlacementFields(source, t.NodeSelector, t.Tolerations, t.RuntimeClassName); len(errs) > 0 {
		paths := make([]string, len(errs))
		for i, e := range errs {
			paths[i] = e.Path
		}
		s.agentLifecycleLog.Warn("Kubernetes placement: the flat instance's runtime_target placement is not valid", "agent", agentName, "fields", paths)
		return nil, placementRefusal(placementFlatInvalidCause)
	}
	rp := &api.ResolvedPlacement{
		NodeSelector:     config.MergeNodeSelector(t.NodeSelector, nil),
		Tolerations:      config.MergeTolerations(t.Tolerations, nil),
		RuntimeClassName: t.RuntimeClassName,
		Sources:          map[string][]string{},
		OperatorOwned:    true,
	}
	if len(rp.NodeSelector) > 0 {
		rp.Sources["node_selector"] = []string{source}
	}
	if len(rp.Tolerations) > 0 {
		rp.Sources["tolerations"] = []string{source}
	}
	if rp.RuntimeClassName != "" {
		rp.Sources["runtime_class_name"] = []string{source}
	}
	if rp.InEffect() {
		s.agentLifecycleLog.Debug("Kubernetes placement resolved", "agent", agentName,
			"nodeSelector", rp.NodeSelector, "tolerations", rp.Tolerations, "runtimeClassName", rp.RuntimeClassName, "sources", rp.Sources)
	}
	return rp, nil
}

// checkFlatCreatePlacement checks, for a create on a flat instance, the
// request's own kubernetes blocks (its inline config, which provisioning
// writes into the agent's config, and its create config) against the
// instance's operator-owned placement (config.CheckAgentPlacement), before
// the create has any side effect. Nil on a legacy Runtime Broker, whose
// create relies on Manager.Start's check.
func (s *Server) checkFlatCreatePlacement(req CreateAgentRequest) error {
	if !s.isFlat() {
		return nil
	}
	rp, err := s.resolveFlatPlacement(nil, req.Name)
	if err != nil || rp == nil {
		return err
	}
	var blocks []*api.KubernetesConfig
	if req.InlineConfig != nil {
		blocks = append(blocks, req.InlineConfig.Kubernetes)
	}
	if req.Config != nil {
		blocks = append(blocks, req.Config.Kubernetes)
	}
	for _, block := range blocks {
		if err := config.CheckAgentPlacement(block, rp, config.AgentPlacementLayerRequest); err != nil {
			s.agentLifecycleLog.Warn("Kubernetes placement: the request's kubernetes block conflicts with the instance's placement", "agent", req.Name, "error", err)
			return placementRefusal(err.Error())
		}
	}
	return nil
}

// profileRungCause is the fixed cause for a profile rung that could apply
// but cannot be determined, by how the runtime was chosen. entries are the
// matching runtime entries in sorted order; the first is named as the entry
// to move placement onto.
func (s *Server) profileRungCause(res runtimeResolution, profileGlobal bool, entries, pp []string) string {
	switch res.How {
	case resolutionUnrecorded:
		entry := "<entry>"
		if len(entries) > 0 {
			entry = entries[0]
		}
		return fmt.Sprintf("agent %s has no recorded profile; profile-level placement is configured for its runtime (profiles %s). Re-provision it with a profile (reincarnate, or delete and re-create with --profile <name>), or move that placement onto the runtime entry (runtimes.%s), which applies whatever the profile",
			res.Agent, strings.Join(pp, ", "), entry)
	case resolutionProfile:
		if !profileGlobal {
			return fmt.Sprintf("profile %s is not defined in the agent runtime's settings, and profile-level placement is configured for this runtime; define %s there or move the placement onto the runtime entry", res.Profile, res.Profile)
		}
		return fmt.Sprintf("profile %s's placement targets a different runtime than the one chosen", res.Profile)
	case resolutionDefaultFallback:
		return "the agent's profile could not be determined (the selection profile or its runtime is not defined in the settings), and profile-level placement is configured for this runtime"
	default:
		return "the agent's profile could not be determined (the runtime choice was not classified), and profile-level placement is configured for this runtime"
	}
}

// sortedNames returns the names in a set, sorted.
func sortedNames(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// unionSet returns the union of two name sets.
func unionSet(a, b map[string]bool) map[string]bool {
	out := make(map[string]bool, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// kubernetesPlacementIdentity reads what the placement matcher needs from a
// Kubernetes runtime: the client's current context, the runtime's default
// namespace, whether it has a client, and the client's connection mode. A
// runtime of another type has no client, so every entry is Undetermined.
func kubernetesPlacementIdentity(rt scionrt.Runtime) (ctx, ns string, hasClient bool, conn config.KubeConnection) {
	krt, ok := rt.(*scionrt.KubernetesRuntime)
	if !ok {
		return "", "", false, config.KubeConnUnknown
	}
	if krt.Client == nil {
		return "", krt.DefaultNamespace, false, config.KubeConnUnknown
	}
	return krt.Client.CurrentContext, krt.DefaultNamespace, true, scionrt.KubeConnectionOf(krt.Client.Connection)
}

// memoLiveCurrentContext reads the kubeconfig current-context at most once
// per resolution, with the loading rules the runtime's client uses.
func memoLiveCurrentContext() func() (string, error) {
	var (
		once sync.Once
		ctx  string
		err  error
	)
	return func() (string, error) {
		once.Do(func() { ctx, err = k8s.LiveCurrentContext(os.Getenv("KUBECONFIG")) })
		return ctx, err
	}
}

// warnPlacementOffKubernetes logs, for a start on a runtime other than
// Kubernetes, that the placement set on its runtime entry or profile has no
// effect. Best effort: a settings load error is not reported here and never
// fails the start.
func (s *Server) warnPlacementOffKubernetes(ctx context.Context, res runtimeResolution) {
	if res.Runtime == nil {
		return
	}
	gvs, _, err := config.LoadGlobalSettingsWithOverlayFor(ctx)
	if err != nil || gvs == nil {
		return
	}
	for _, name := range []string{res.Record.EntryName, res.DispatchEntry} {
		if rt, ok := gvs.Runtimes[name]; ok && name != "" && config.RuntimeEntryHasPlacement(rt) {
			s.agentLifecycleLog.Warn("Kubernetes placement on a runtime entry is ignored: the runtime is not Kubernetes",
				"agent", res.Agent, "entry", name, "runtime", res.Runtime.Name())
		}
	}
	if p, ok := gvs.Profiles[res.Profile]; ok && res.Profile != "" && config.ProfileHasPlacement(p) {
		s.agentLifecycleLog.Warn("Kubernetes placement on a profile is ignored: the runtime is not Kubernetes",
			"agent", res.Agent, "profile", res.Profile, "runtime", res.Runtime.Name())
	}
}

// warnProjectPlacement logs the placement values the project's settings
// set: placement is read from the global settings only, so they are never
// applied.
func (s *Server) warnProjectPlacement(agentName string, gvs, projectVS *config.VersionedSettings) {
	if projectVS == nil {
		return
	}
	for name, rt := range projectVS.Runtimes {
		if !config.RuntimeEntryHasPlacement(rt) {
			continue
		}
		if g, ok := gvs.Runtimes[name]; ok && samePlacement(g.NodeSelector, g.Tolerations, g.RuntimeClassName, rt.NodeSelector, rt.Tolerations, rt.RuntimeClassName) {
			continue
		}
		s.agentLifecycleLog.Warn("Kubernetes placement set in project settings is ignored; it is read from the agent runtime's global settings",
			"agent", agentName, "entry", "runtimes."+name)
	}
	for name, p := range projectVS.Profiles {
		if !config.ProfileHasPlacement(p) {
			continue
		}
		if g, ok := gvs.Profiles[name]; ok && samePlacement(g.NodeSelector, g.Tolerations, g.RuntimeClassName, p.NodeSelector, p.Tolerations, p.RuntimeClassName) {
			continue
		}
		s.agentLifecycleLog.Warn("Kubernetes placement set in project settings is ignored; it is read from the agent runtime's global settings",
			"agent", agentName, "entry", "profiles."+name)
	}
}

func samePlacement(aSel map[string]string, aTol []api.K8sToleration, aRC string, bSel map[string]string, bTol []api.K8sToleration, bRC string) bool {
	return config.SiblingPlacementAgrees(
		config.V1RuntimeConfig{NodeSelector: aSel, Tolerations: aTol, RuntimeClassName: aRC},
		config.V1RuntimeConfig{NodeSelector: bSel, Tolerations: bTol, RuntimeClassName: bRC})
}

// checkStartLegacyPlacement checks, before a start writes its inline config
// update, that the agent's kubernetes block as that update would leave it
// (the persisted scion-agent.json merged with inlineConfig, as
// applyInlineConfigUpdate merges them) is allowed while placement is in
// effect (config.CheckLegacyPlacement). When the project directory or the
// file cannot be resolved, read or parsed, applyInlineConfigUpdate writes
// nothing, so the inline block alone is checked; Manager.Start checks the
// block it loads as a backstop.
func (s *Server) checkStartLegacyPlacement(agentName, projectPath string, inlineConfig *api.ScionConfig, sharedWorkspace bool, rp *api.ResolvedPlacement) error {
	if rp != nil && rp.OperatorOwned {
		return s.checkStartOperatorPlacement(agentName, projectPath, inlineConfig, sharedWorkspace, rp)
	}
	if !rp.InEffect() {
		return nil
	}
	var block *api.KubernetesConfig
	var loaded loadedAgentConfig
	err := errors.New("no project path")
	if projectPath != "" {
		loaded, err = readAgentScionConfig(agentName, projectPath, sharedWorkspace)
	}
	switch {
	case err != nil:
		if inlineConfig != nil {
			block = inlineConfig.Kubernetes
		}
	case inlineConfig != nil:
		block = config.MergeScionConfig(loaded.cfg, inlineConfig).Kubernetes
	default:
		block = loaded.cfg.Kubernetes
	}
	if err := config.CheckLegacyPlacement(block, rp); err != nil {
		s.agentLifecycleLog.Warn("Kubernetes placement: the agent's own kubernetes block conflicts with it", "agent", agentName, "error", err)
		return placementRefusal(err.Error())
	}
	return nil
}

// checkRestartLegacyPlacement checks, before the restart endpoint's stop,
// the agent's persisted kubernetes block against the placement in effect. A
// config that cannot be read fails closed.
func (s *Server) checkRestartLegacyPlacement(agentName, projectPath string, sharedWorkspace bool, rp *api.ResolvedPlacement) error {
	if rp != nil && rp.OperatorOwned {
		if projectPath == "" {
			// Without a project path the stored config cannot be read
			// before the stop, and operator-owned placement must be
			// checked before it (the request names no project, and no
			// container of the project recorded one).
			s.agentLifecycleLog.Warn("Kubernetes placement: refusing a restart whose stored config cannot be checked before the stop", "agent", agentName)
			return placementRefusal(placementFlatRestartNoProjectCause)
		}
		return s.checkRestartOperatorPlacement(agentName, projectPath, sharedWorkspace, rp)
	}
	if !rp.InEffect() || projectPath == "" {
		// Without a project path there is no persisted config to check;
		// Manager.Start checks the block it loads.
		return nil
	}
	loaded, err := readAgentScionConfig(agentName, projectPath, sharedWorkspace)
	if err != nil {
		s.agentLifecycleLog.Warn("Kubernetes placement: the agent's config could not be read", "agent", agentName, "stage", loaded.stage, "error", err)
		return placementRefusal(placementAgentConfigCause)
	}
	if err := config.CheckLegacyPlacement(loaded.cfg.Kubernetes, rp); err != nil {
		s.agentLifecycleLog.Warn("Kubernetes placement: the agent's own kubernetes block conflicts with it", "agent", agentName, "error", err)
		return placementRefusal(err.Error())
	}
	return nil
}

// checkStartOperatorPlacement is checkStartLegacyPlacement for
// operator-owned placement (config.CheckAgentPlacement), run even when no
// placement is in effect. The request's own block is checked first, so a
// conflict it brings is reported as the request's; then the block the
// inline update would leave (the stored config merged with the request, as
// applyInlineConfigUpdate merges them), whose remaining conflicts come
// from the stored config. A stored config that exists but cannot be read
// or parsed fails closed, as on restart. One that does not exist yet (an
// agent this Runtime Broker has not provisioned) leaves the request's block
// checked: the inline update writes nothing then, and Manager.Start checks
// the config it provisions.
func (s *Server) checkStartOperatorPlacement(agentName, projectPath string, inlineConfig *api.ScionConfig, sharedWorkspace bool, rp *api.ResolvedPlacement) error {
	refuse := func(err error) error {
		s.agentLifecycleLog.Warn("Kubernetes placement: the agent's own kubernetes block conflicts with it", "agent", agentName, "error", err)
		return placementRefusal(err.Error())
	}
	if inlineConfig != nil {
		if err := config.CheckAgentPlacement(inlineConfig.Kubernetes, rp, config.AgentPlacementLayerRequest); err != nil {
			return refuse(err)
		}
	}
	if projectPath == "" {
		return nil
	}
	loaded, err := readAgentScionConfig(agentName, projectPath, sharedWorkspace)
	if err != nil {
		if loaded.stage == "read" && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		s.agentLifecycleLog.Warn("Kubernetes placement: the agent's config could not be read", "agent", agentName, "stage", loaded.stage, "error", err)
		return placementRefusal(placementAgentConfigCause)
	}
	block := loaded.cfg.Kubernetes
	if inlineConfig != nil {
		block = config.MergeScionConfig(loaded.cfg, inlineConfig).Kubernetes
	}
	if err := config.CheckAgentPlacement(block, rp, config.AgentPlacementLayerStored); err != nil {
		return refuse(err)
	}
	return nil
}

// checkRestartOperatorPlacement is checkRestartLegacyPlacement for
// operator-owned placement, run even when no placement is in effect: the
// agent's stored kubernetes block is checked before the restart's stop. A
// config that cannot be read fails closed.
func (s *Server) checkRestartOperatorPlacement(agentName, projectPath string, sharedWorkspace bool, rp *api.ResolvedPlacement) error {
	loaded, err := readAgentScionConfig(agentName, projectPath, sharedWorkspace)
	if err != nil {
		s.agentLifecycleLog.Warn("Kubernetes placement: the agent's config could not be read", "agent", agentName, "stage", loaded.stage, "error", err)
		return placementRefusal(placementAgentConfigCause)
	}
	if err := config.CheckAgentPlacement(loaded.cfg.Kubernetes, rp, config.AgentPlacementLayerStored); err != nil {
		s.agentLifecycleLog.Warn("Kubernetes placement: the agent's own kubernetes block conflicts with it", "agent", agentName, "error", err)
		return placementRefusal(err.Error())
	}
	return nil
}
