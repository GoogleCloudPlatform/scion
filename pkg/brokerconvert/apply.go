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

package brokerconvert

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/capacityplan"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// recentHeartbeat is how recent a legacy row's heartbeat must be for an
// "online" status to count as a running server (P-4).
const recentHeartbeat = 2 * time.Minute

// Env is what apply, status and rollback use from the host and the Hub.
type Env struct {
	// GlobalDir is the global Scion directory.
	GlobalDir string
	// Client is the operator's Hub client.
	Client hubclient.Client
	// Register registers one planned instance (step 1): it loads or mints
	// the instance identity, registers and joins it, checks both
	// acknowledgements and only then saves its credentials. It returns
	// the Runtime Broker ID and the runtime target ID.
	Register func(ctx context.Context, inst config.V1RuntimeBrokerInstanceConfig) (brokerID, runtimeTargetID string, err error)
	// ServerRunning is the local half of P-4: whether this host's server
	// (its Runtime Broker status endpoint) answers.
	ServerRunning func(ctx context.Context) bool
	// LoadInstances strictly reads the current server.broker.instances.
	LoadInstances func() ([]config.V1RuntimeBrokerInstanceConfig, error)
	// ReadSettings reads the global settings file, for the evidence copy.
	ReadSettings func() ([]byte, error)
	// WriteInstances and RemoveInstances are the config writer and its
	// inverse (config.WriteRuntimeBrokerInstances and
	// config.RemoveRuntimeBrokerInstances on GlobalDir).
	WriteInstances  func([]config.V1RuntimeBrokerInstanceConfig) error
	RemoveInstances func(expected []config.V1RuntimeBrokerInstanceConfig) error
	// BeforeMove, if set, runs for each instance after its row is
	// deregistered (or confirmed absent) and before its directory is
	// moved aside. An error stops the rollback before that move.
	BeforeMove func(globalDir, key string) error
	// Now is the clock.
	Now func() time.Time
}

func (e *Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// ApplyInput is what apply converts.
type ApplyInput struct {
	// Plan is the plan built by BuildLocal and CollectHub, the same
	// builder and inputs as --plan.
	Plan *Plan
	// Choices are the reviewed choices the plan was built with.
	Choices []config.ConversionChoice
	// LegacyDefaultProfile is the legacy settings' default profile, for a
	// project with no active profile.
	LegacyDefaultProfile string
}

// run is one locked operation: the env, the journal and its report.
type run struct {
	ctx context.Context
	env *Env
	j   *Journal
}

func (r *run) save() *Refusal {
	if err := SaveJournal(r.env.GlobalDir, r.j, r.env.now()); err != nil {
		return refuse(RefuseLocalWrite)
	}
	return nil
}

// Apply converts the drained legacy host to the plan's instance (design
// sections 4.1 to 4.7, one instance, remote registration). It holds the
// host lock for its whole run. A refusal is returned in the report and as
// the error; the journal stays resumable.
func Apply(ctx context.Context, env *Env, in ApplyInput) (*Report, error) {
	lock, err := AcquireHostLock(env.GlobalDir)
	if err != nil {
		if errors.Is(err, ErrHostLockHeld) {
			return refused(ModeApply, nil, refuse(RefuseLockHeld))
		}
		return refused(ModeApply, nil, refuse(RefuseLocalWrite))
	}
	defer lock.Release()

	p := in.Plan
	// P-1: a clean plan for exactly one instance, from the Hub too.
	if p == nil || p.LegacyBrokerID == "" || p.Agents == nil || p.Projects == nil {
		return refused(ModeApply, nil, refuse(RefuseHubSections))
	}
	if p.ExitCode() != ExitClean || p.Agents.Note != "" || p.Projects.Note != "" {
		return refused(ModeApply, nil, refuse(RefusePlanNotClean))
	}
	insts := p.SettingsInstances()
	if len(insts) != 1 || len(p.Instances) != 1 {
		return refused(ModeApply, nil, refuse(RefuseMultipleInstances))
	}
	// The capacity carry-over (erratum E2 to sections 4.2 and 7): the new
	// row takes the legacy row's own explicit override, L for one
	// instance, 0 (unlimited) for no_limit; never unset.
	maxAgents, ref := capacityTarget(p)
	if ref != nil {
		return refused(ModeApply, nil, ref)
	}
	digest, err := PlanDigest(p, in.Choices)
	if err != nil {
		return refused(ModeApply, nil, refuse(RefuseLocalWrite))
	}

	// The journal: resume only on an equal digest (section 5.2).
	j, err := LoadJournal(env.GlobalDir)
	if err != nil {
		return refused(ModeApply, nil, refuse(RefuseLocalWrite))
	}
	resuming := false
	if j != nil {
		switch j.State {
		case JournalRolledBack:
			j = nil // a finished rollback: a new run
		case JournalRollingBack:
			return refused(ModeApply, j, refuse(RefuseRollingBack))
		default:
			if j.LegacyBrokerID != p.LegacyBrokerID {
				return refused(ModeApply, j, refuse(RefuseOtherLegacy))
			}
			if j.PlanDigest != digest {
				return refused(ModeApply, j, refuse(RefusePlanChanged))
			}
			resuming = true
		}
	}

	// P-2: not already flat, unless this apply wrote the settings.
	current, err := env.LoadInstances()
	if err != nil {
		return refused(ModeApply, j, refuse(RefuseSettingsWrite))
	}
	if len(current) > 0 && (!resuming || j.Settings.State == StepPending || !config.InstancesEqual(current, insts)) {
		return refused(ModeApply, j, refuse(RefuseAlreadyFlat))
	}
	if resuming && j.State == JournalApplied {
		rep := reportFrom(ModeApply, j)
		rep.Notes = append(rep.Notes, NoteNoop)
		return rep, nil
	}

	r := &run{ctx: ctx, env: env, j: j}
	// P-3 and P-4.
	if ref := r.drainedAndStopped(p.LegacyBrokerID); ref != nil {
		return refused(ModeApply, j, ref)
	}
	// P-5: the capacity write needs quota.update (hub-scoped), read from
	// the legacy row's settings capabilities before anything is written.
	if ref := r.checkCapacityPermission(p.LegacyBrokerID); ref != nil {
		return refused(ModeApply, j, ref)
	}
	// P-6: no name collision, other than a row this run registered.
	if ref := r.checkNames(insts); ref != nil {
		return refused(ModeApply, j, ref)
	}
	// P-7 and the per-project facts for step 3.
	projects, ref := r.collectProjects(p.LegacyBrokerID)
	if ref != nil {
		return refused(ModeApply, j, ref)
	}

	if r.j == nil {
		if r.j, err = NewJournal(digest, p.LegacyBrokerID, env.now()); err != nil {
			return refused(ModeApply, nil, refuse(RefuseLocalWrite))
		}
		for _, inst := range insts {
			target := maxAgents
			r.j.Register = append(r.j.Register, RegisterItem{Key: inst.Key, Name: inst.Name, ItemStatus: ItemStatus{State: StepPending},
				MaxAgents: &target, Capacity: ItemStatus{State: StepPending}})
		}
	}
	// The evidence copy and the copy of the instances step 2 writes.
	if ref := r.keepCopies(insts); ref != nil {
		return refused(ModeApply, r.j, ref)
	}

	// Step 1: register (the first Hub write, behind the Hub's experiment
	// gate and name check).
	for i := range r.j.Register {
		if ref := r.register(&r.j.Register[i], insts); ref != nil {
			return refused(ModeApply, r.j, ref)
		}
	}
	// Step 1b: each instance row's agent limit, written and read back
	// before step 3, so the row never serves agents without it.
	for i := range r.j.Register {
		if ref := r.carryCapacity(&r.j.Register[i]); ref != nil {
			return refused(ModeApply, r.j, ref)
		}
	}
	// Step 2: the settings.
	if ref := r.writeSettings(insts); ref != nil {
		return refused(ModeApply, r.j, ref)
	}
	// Drained re-check before step 3 (section 4.6).
	if ref := r.drainedAndStopped(p.LegacyBrokerID); ref != nil {
		return refused(ModeApply, r.j, ref)
	}
	// Step 3: re-point projects.
	if ref := r.repointProjects(p, in.LegacyDefaultProfile, projects); ref != nil {
		return refused(ModeApply, r.j, ref)
	}
	// Drained re-check before the switch instruction.
	if ref := r.drainedAndStopped(p.LegacyBrokerID); ref != nil {
		return refused(ModeApply, r.j, ref)
	}
	r.j.Switch = ItemStatus{State: StepDone}
	r.j.State = JournalApplied
	if ref := r.save(); ref != nil {
		return refused(ModeApply, r.j, ref)
	}
	rep := reportFrom(ModeApply, r.j)
	rep.Notes = append(rep.Notes, NoteEvidenceKept, NoteLegacyLinks, NoteSwitch)
	return rep, nil
}

// refused returns the report of a refusal and the refusal as the error.
func refused(mode string, j *Journal, ref *Refusal) (*Report, error) {
	rep := reportFrom(mode, j)
	rep.Refusal = ref
	return rep, ref
}

// drainedAndStopped is P-3 (no non-terminal agent on the legacy row) and
// P-4 (the server is not running, and the legacy row is not online with a
// recent heartbeat).
func (r *run) drainedAndStopped(legacyID string) *Refusal {
	if r.env.ServerRunning != nil && r.env.ServerRunning(r.ctx) {
		return refuse(RefuseServerRunning)
	}
	row, err := r.env.Client.RuntimeBrokers().Get(r.ctx, legacyID)
	if err == nil && row == nil {
		err = errEmptyResponse
	}
	if err != nil {
		return refuseHub(RefuseHubRequest, err)
	}
	if strings.EqualFold(row.Status, "online") && r.env.now().Sub(row.LastHeartbeat) < recentHeartbeat {
		return refuse(RefuseServerRunning)
	}
	return r.drained(legacyID, RefuseNotDrained)
}

// drained refuses with code when a non-terminal agent is bound to brokerID.
func (r *run) drained(brokerID, code string) *Refusal {
	opts := &hubclient.ListAgentsOptions{RuntimeBrokerID: brokerID}
	var blocking []AgentEntry
	seen := map[string]bool{}
	for page := 0; ; page++ {
		if page == maxAgentPages {
			return refuse(RefuseHubRequest)
		}
		resp, err := r.env.Client.Agents().List(r.ctx, opts)
		if err == nil && resp == nil {
			err = errEmptyResponse
		}
		if err != nil {
			return refuseHub(RefuseHubRequest, err)
		}
		for _, a := range resp.Agents {
			if seen[a.ID] || isTerminalPhase(a.Phase) {
				continue
			}
			seen[a.ID] = true
			blocking = append(blocking, AgentEntry{ID: a.ID, Name: a.Name, ProjectID: a.ProjectID, Project: a.Project, Phase: a.Phase})
		}
		if resp.Page.NextCursor == "" || resp.Page.NextCursor == opts.Page.Cursor {
			break
		}
		opts.Page.Cursor = resp.Page.NextCursor
	}
	if len(blocking) > 0 {
		ref := refuse(code)
		ref.Agents = blocking
		return ref
	}
	return nil
}

// checkNames is P-6: no existing row has a planned instance's name or
// slug, other than the row this run already registered for that key.
func (r *run) checkNames(insts []config.V1RuntimeBrokerInstanceConfig) *Refusal {
	own := map[string]bool{}
	if r.j != nil {
		for _, it := range r.j.Register {
			if it.BrokerID != "" {
				own[it.BrokerID] = true
			}
		}
	}
	var conflicts []string
	for _, inst := range insts {
		resp, err := r.env.Client.RuntimeBrokers().List(r.ctx, &hubclient.ListBrokersOptions{Name: inst.Name})
		if err == nil && resp == nil {
			err = errEmptyResponse
		}
		if err != nil {
			return refuseHub(RefuseHubRequest, err)
		}
		for _, b := range resp.Brokers {
			if own[b.ID] {
				continue
			}
			// The list is filtered by name only (the API has no slug
			// filter), so a slug collision on a row of another name is not
			// seen here: the Hub refuses it at registration (R2), in step 1,
			// before the settings change.
			if strings.EqualFold(b.Name, inst.Name) {
				conflicts = append(conflicts, inst.Key)
				break
			}
		}
	}
	if len(conflicts) > 0 {
		ref := refuse(RefuseNameConflict)
		ref.Keys = conflicts
		return ref
	}
	return nil
}

// projectFacts are the Hub facts step 3 needs for one project.
type projectFacts struct {
	id            string
	defaultID     string
	activeProfile string
	providers     map[string]bool
}

// collectProjects is P-7 plus the facts for step 3: every project the
// legacy row provides, its providers, its default and its active profile.
func (r *run) collectProjects(legacyID string) ([]projectFacts, *Refusal) {
	c := r.env.Client
	resp, err := c.RuntimeBrokers().ListProjects(r.ctx, legacyID)
	if err == nil && resp == nil {
		err = errEmptyResponse
	}
	if err != nil {
		return nil, refuseHub(RefuseHubRequest, err)
	}
	var out []projectFacts
	var pathLinked []string
	for _, info := range resp.Projects {
		f := projectFacts{id: info.ProjectID, providers: map[string]bool{}}
		provs, err := c.Projects().ListProviders(r.ctx, info.ProjectID)
		if err == nil && provs == nil {
			err = errEmptyResponse
		}
		if err != nil {
			return nil, refuseHub(RefuseHubRequest, err)
		}
		for _, pv := range provs.Providers {
			f.providers[pv.BrokerID] = true
			if pv.BrokerID == legacyID && pv.LocalPath != "" {
				pathLinked = append(pathLinked, info.ProjectID)
			}
		}
		proj, err := c.Projects().Get(r.ctx, info.ProjectID)
		if err == nil && proj == nil {
			err = errEmptyResponse
		}
		if err != nil {
			return nil, refuseHub(RefuseHubRequest, err)
		}
		f.defaultID = proj.DefaultRuntimeBrokerID
		settings, err := c.Projects().GetSettings(r.ctx, info.ProjectID)
		if err == nil && settings == nil {
			err = errEmptyResponse
		}
		if err != nil {
			return nil, refuseHub(RefuseHubRequest, err)
		}
		if settings.ActiveProfile != nil {
			f.activeProfile = *settings.ActiveProfile
		}
		out = append(out, f)
	}
	if len(pathLinked) > 0 {
		sort.Strings(pathLinked)
		ref := refuse(RefuseLocalPath)
		ref.Projects = pathLinked
		return nil, ref
	}
	sort.Slice(out, func(i, k int) bool { return out[i].id < out[k].id })
	return out, nil
}

// keepCopies writes the evidence copy of the pre-apply settings and the
// copy of the instances step 2 writes, once per run.
func (r *run) keepCopies(insts []config.V1RuntimeBrokerInstanceConfig) *Refusal {
	if r.j.EvidenceFile == "" {
		data, err := r.env.ReadSettings()
		if err != nil {
			return refuse(RefuseLocalWrite)
		}
		if r.j.EvidenceFile, err = WriteEvidenceCopy(r.env.GlobalDir, r.j.RunID, data); err != nil {
			return refuse(RefuseLocalWrite)
		}
	}
	if r.j.InstancesFile == "" {
		data, err := yamlv3.Marshal(insts)
		if err != nil {
			return refuse(RefuseLocalWrite)
		}
		if r.j.InstancesFile, err = WriteInstancesCopy(r.env.GlobalDir, r.j.RunID, data); err != nil {
			return refuse(RefuseLocalWrite)
		}
	}
	return r.save()
}

// register is step 1 for one instance: started, the registration, done.
// A re-run re-registers by the saved identity (idempotent).
func (r *run) register(it *RegisterItem, insts []config.V1RuntimeBrokerInstanceConfig) *Refusal {
	if it.State == StepDone {
		return nil
	}
	var inst *config.V1RuntimeBrokerInstanceConfig
	for i := range insts {
		if insts[i].Key == it.Key {
			inst = &insts[i]
		}
	}
	if inst == nil {
		return refuse(RefusePlanChanged)
	}
	it.ItemStatus = ItemStatus{State: StepStarted}
	if ref := r.save(); ref != nil {
		return ref
	}
	brokerID, targetID, err := r.env.Register(r.ctx, *inst)
	if err != nil {
		ref := refuseHub(RefuseRegister, err)
		it.Error = fixedError(ref)
		// The Hub may hold a row for this instance already (registered, then
		// join, an acknowledgement or the credential save failed): record
		// the ID the identity file carries, so rollback deregisters it.
		if id, ok, ierr := identityBrokerID(r.env.GlobalDir, it.Key); ierr == nil && ok {
			it.BrokerID = id
		}
		if sref := r.save(); sref != nil {
			return sref
		}
		return ref
	}
	it.BrokerID, it.RuntimeTargetID = brokerID, targetID
	it.ItemStatus = ItemStatus{State: StepDone}
	return r.save()
}

// writeSettings is step 2: server.broker.instances, verbatim.
func (r *run) writeSettings(insts []config.V1RuntimeBrokerInstanceConfig) *Refusal {
	if r.j.Settings.State == StepDone {
		return nil
	}
	// The instances copy is what rollback compares the settings against,
	// so it must equal what this step writes: a resume may carry a changed
	// list (a rotated env value leaves the plan digest equal).
	if r.j.InstancesFile != "" {
		data, err := yamlv3.Marshal(insts)
		if err != nil {
			return refuse(RefuseLocalWrite)
		}
		if err := ReplaceInstancesCopy(r.env.GlobalDir, r.j.InstancesFile, data); err != nil {
			return refuse(RefuseLocalWrite)
		}
	}
	r.j.Settings = ItemStatus{State: StepStarted}
	if ref := r.save(); ref != nil {
		return ref
	}
	if err := r.env.WriteInstances(insts); err != nil {
		ref := refuse(RefuseSettingsWrite)
		var mm *config.InstancesMismatchError
		if errors.As(err, &mm) {
			ref.Keys = mm.Keys
		}
		r.j.Settings.Error = fixedError(ref)
		if sref := r.save(); sref != nil {
			return sref
		}
		return ref
	}
	r.j.Settings = ItemStatus{State: StepDone}
	return r.save()
}

// instanceFor maps a profile to its planned instance key.
func instanceFor(p *Plan, profile string) string {
	if profile == "" {
		return ""
	}
	for _, m := range p.ProfileMap {
		if m.Profile == profile {
			return m.Instance
		}
	}
	return ""
}

// repointProjects is step 3: each project gets a provider link to every
// mapped instance, and a default that pointed at the legacy row is set
// to the instance of the project's active profile (or of the legacy
// default profile). The legacy link is kept.
func (r *run) repointProjects(p *Plan, legacyDefaultProfile string, projects []projectFacts) *Refusal {
	brokerByKey := map[string]string{}
	for _, it := range r.j.Register {
		brokerByKey[it.Key] = it.BrokerID
	}
	var linkAll []string
	seen := map[string]bool{}
	for _, m := range p.ProfileMap {
		if id := brokerByKey[m.Instance]; m.Instance != "" && id != "" && !seen[id] {
			seen[id] = true
			linkAll = append(linkAll, id)
		}
	}
	sort.Strings(linkAll)

	items := map[string]*ProjectItem{}
	for i := range r.j.Projects {
		items[r.j.Projects[i].ProjectID] = &r.j.Projects[i]
	}
	c := r.env.Client.Projects()
	for _, f := range projects {
		it := items[f.id]
		if it == nil {
			r.j.Projects = append(r.j.Projects, ProjectItem{ProjectID: f.id, OldDefault: f.defaultID, Linked: []string{}, ItemStatus: ItemStatus{State: StepPending}})
			// The append may move the slice; rebuild the index.
			items = map[string]*ProjectItem{}
			for i := range r.j.Projects {
				items[r.j.Projects[i].ProjectID] = &r.j.Projects[i]
			}
			it = items[f.id]
		}
		if it.State == StepDone {
			continue
		}
		newDefault := ""
		if f.defaultID == r.j.LegacyBrokerID || (it.NewDefault != "" && f.defaultID == it.NewDefault) {
			key := instanceFor(p, f.activeProfile)
			if key == "" {
				key = instanceFor(p, legacyDefaultProfile)
			}
			if key == "" && len(r.j.Register) == 1 {
				key = r.j.Register[0].Key
			}
			if newDefault = brokerByKey[key]; newDefault == "" {
				ref := refuse(RefuseDefaultUnknown)
				ref.Projects = []string{f.id}
				return ref
			}
		}
		it.ItemStatus = ItemStatus{State: StepStarted}
		if ref := r.save(); ref != nil {
			return ref
		}
		for _, id := range linkAll {
			if !f.providers[id] {
				if _, err := c.AddProvider(r.ctx, f.id, &hubclient.AddProviderRequest{BrokerID: id}); err != nil {
					return r.failProject(it, refuseHub(RefuseHubRequest, err))
				}
			}
			if !contains(it.Linked, id) {
				it.Linked = append(it.Linked, id)
			}
		}
		if newDefault != "" && f.defaultID != newDefault {
			if _, err := c.Update(r.ctx, f.id, &hubclient.UpdateProjectRequest{DefaultRuntimeBrokerID: newDefault}); err != nil {
				return r.failProject(it, refuseHub(RefuseHubRequest, err))
			}
		}
		if newDefault != "" {
			it.NewDefault = newDefault
		}
		// Read-back.
		provs, err := c.ListProviders(r.ctx, f.id)
		if err == nil && provs == nil {
			err = errEmptyResponse
		}
		if err != nil {
			return r.failProject(it, refuseHub(RefuseHubRequest, err))
		}
		have := map[string]bool{}
		for _, pv := range provs.Providers {
			have[pv.BrokerID] = true
		}
		for _, id := range linkAll {
			if !have[id] {
				return r.failProject(it, refuse(RefuseHubRequest))
			}
		}
		if newDefault != "" {
			proj, err := c.Get(r.ctx, f.id)
			if err == nil && proj == nil {
				err = errEmptyResponse
			}
			if err != nil {
				return r.failProject(it, refuseHub(RefuseHubRequest, err))
			}
			if proj.DefaultRuntimeBrokerID != newDefault {
				return r.failProject(it, refuse(RefuseHubRequest))
			}
		}
		it.ItemStatus = ItemStatus{State: StepDone}
		if ref := r.save(); ref != nil {
			return ref
		}
	}
	return nil
}

func (r *run) failProject(it *ProjectItem, ref *Refusal) *Refusal {
	it.Error = fixedError(ref)
	ref.Projects = []string{it.ProjectID}
	if sref := r.save(); sref != nil {
		return sref
	}
	return ref
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// capacityTarget is the agent limit the single instance takes over: the
// legacy row's own explicit override (L, or 0 for unlimited).
func capacityTarget(p *Plan) (int64, *Refusal) {
	c := p.Capacity
	if c.Status != CapacityPlanned {
		return 0, refuse(RefuseCapacityUnknown)
	}
	switch c.Outcome {
	case capacityplan.OutcomeNoLimit:
		return 0, nil
	case capacityplan.OutcomePreserveBySplit:
		if len(c.Allocations) != 1 || c.MaxAgents == nil || c.Allocations[0].MaxAgents != *c.MaxAgents {
			return 0, refuse(RefuseCapacitySplit)
		}
		return c.Allocations[0].MaxAgents, nil
	case capacityplan.OutcomeOperatorDecisionRequired:
		return 0, refuse(RefuseCapacityDecision)
	}
	return 0, refuse(RefuseCapacityUnknown)
}

func (r *run) settingsAPI() (hubclient.RuntimeBrokerSettingsGetter, hubclient.RuntimeBrokerSettingsPutter, bool) {
	brokers := r.env.Client.RuntimeBrokers()
	g, ok1 := brokers.(hubclient.RuntimeBrokerSettingsGetter)
	p, ok2 := brokers.(hubclient.RuntimeBrokerSettingsPutter)
	return g, p, ok1 && ok2
}

// checkCapacityPermission is the up-front check of the capacity write's
// permission: the legacy row's settings capabilities (hub-scoped, so they
// hold for the new row too).
func (r *run) checkCapacityPermission(legacyID string) *Refusal {
	g, _, ok := r.settingsAPI()
	if !ok {
		return refuse(RefuseCapacityUnknown)
	}
	s, err := g.GetSettings(r.ctx, legacyID)
	if err == nil && s == nil {
		err = errEmptyResponse
	}
	if err != nil {
		return refuseHub(RefuseHubRequest, err)
	}
	if !s.Capabilities.Update {
		return refuse(RefuseCapacityPermission)
	}
	return nil
}

// carryCapacity is the capacity item of one instance. Its drift rule is
// decided by the journal: before the item is done an unset value is
// written and an equal value is taken as done; once it is done the value
// must still be the target. Any other value refuses, naming the key.
func (r *run) carryCapacity(it *RegisterItem) *Refusal {
	if it.MaxAgents == nil || it.BrokerID == "" {
		return refuse(RefuseCapacityUnknown)
	}
	target := *it.MaxAgents
	g, put, ok := r.settingsAPI()
	if !ok {
		return refuse(RefuseCapacityUnknown)
	}
	drift := func() *Refusal {
		ref := refuse(RefuseCapacityDrift)
		ref.Keys = []string{it.Key}
		return ref
	}
	read := func() (*hubclient.RuntimeBrokerSettings, *Refusal) {
		s, err := g.GetSettings(r.ctx, it.BrokerID)
		if err == nil && s == nil {
			err = errEmptyResponse
		}
		if err != nil {
			return nil, refuseHub(RefuseHubRequest, err)
		}
		return s, nil
	}
	cur, ref := read()
	if ref != nil {
		return r.failCapacity(it, ref)
	}
	if it.Capacity.State == StepDone {
		if cur.Settings.MaxAgents == nil || *cur.Settings.MaxAgents != target {
			return drift()
		}
		return nil
	}
	if cur.Settings.MaxAgents != nil {
		if *cur.Settings.MaxAgents != target {
			return r.failCapacity(it, drift())
		}
		it.Capacity = ItemStatus{State: StepDone}
		return r.save()
	}
	it.Capacity = ItemStatus{State: StepStarted}
	if ref := r.save(); ref != nil {
		return ref
	}
	// One write, and on a stale revision (409) one re-read: equal is done,
	// still unset is written once more with the fresh revision, anything
	// else refuses. Never a blind retry.
	rev := cur.Revision
	for attempt := 0; ; attempt++ {
		v := target
		_, err := put.PutMaxAgents(r.ctx, it.BrokerID, &v, rev)
		if err == nil {
			break
		}
		if apiclient.IsForbiddenError(err) {
			return r.failCapacity(it, refuseHub(RefuseCapacityPermission, err))
		}
		if !apiclient.IsConflictError(err) || attempt > 0 {
			return r.failCapacity(it, refuseHub(RefuseHubRequest, err))
		}
		again, ref := read()
		if ref != nil {
			return r.failCapacity(it, ref)
		}
		if again.Settings.MaxAgents != nil {
			if *again.Settings.MaxAgents != target {
				return r.failCapacity(it, drift())
			}
			break // someone wrote the same value
		}
		rev = again.Revision
	}
	back, ref := read()
	if ref != nil {
		return r.failCapacity(it, ref)
	}
	if back.Settings.MaxAgents == nil || *back.Settings.MaxAgents != target {
		return r.failCapacity(it, drift())
	}
	it.Capacity = ItemStatus{State: StepDone}
	return r.save()
}

func (r *run) failCapacity(it *RegisterItem, ref *Refusal) *Refusal {
	it.Capacity.Error = fixedError(ref)
	if sref := r.save(); sref != nil {
		return sref
	}
	return ref
}
