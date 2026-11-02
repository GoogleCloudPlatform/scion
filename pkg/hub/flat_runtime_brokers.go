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
	"log/slog"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/brokerownership"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Flat Runtime Broker Hub logic (.design/flat-runtime-brokers-contract.md).
// Every refusal here is a correctness or compatibility refusal: dispatch is
// authorized only by canDispatchToBroker, and registration only by the
// existing registration gates, which always run first.

// --- refusals ------------------------------------------------------------

func experimentDisabledRefusal() *RuntimeTargetRefusal {
	return &RuntimeTargetRefusal{
		Code:    ErrCodeExperimentDisabled,
		Status:  http.StatusPreconditionFailed,
		Message: fmt.Sprintf("flat Runtime Brokers are not enabled on this Hub (experiment %s is off)", experiments.FlatRuntimeBrokers),
		Details: map[string]interface{}{"experiment": experiments.FlatRuntimeBrokers},
	}
}

func runtimeTargetChangedRefusal(brokerID, stored, reported string) *RuntimeTargetRefusal {
	return &RuntimeTargetRefusal{
		Code:   ErrCodeRuntimeTargetChanged,
		Status: http.StatusConflict,
		Message: fmt.Sprintf("Runtime Broker %s is bound to runtime target %q, but the request reported %q; a Runtime Broker's runtime target never changes",
			brokerID, stored, reported),
		Details: map[string]interface{}{
			"runtimeBrokerId":         brokerID,
			"storedRuntimeTargetId":   stored,
			"reportedRuntimeTargetId": reported,
		},
	}
}

func runtimeBrokerNotFlatRefusal(brokerID string) *RuntimeTargetRefusal {
	return &RuntimeTargetRefusal{
		Code:   ErrCodeRuntimeBrokerNotFlat,
		Status: http.StatusConflict,
		Message: fmt.Sprintf("Runtime Broker %s is a profile-based Runtime Broker; it is never converted into a flat Runtime Broker implicitly",
			brokerID),
		Details: map[string]interface{}{"runtimeBrokerId": brokerID},
	}
}

// runtimeBrokerNameConflictRefusal carries only the requested name and slug,
// for every caller.
func runtimeBrokerNameConflictRefusal(name, slug string) *RuntimeTargetRefusal {
	return &RuntimeTargetRefusal{
		Code:    ErrCodeRuntimeBrokerNameConflict,
		Status:  http.StatusConflict,
		Message: fmt.Sprintf("Runtime Broker name %q (slug %q) is already in use; choose another name", name, slug),
		Details: map[string]interface{}{"name": name, "slug": slug},
	}
}

// runtimeTargetInUseRefusal refuses a flat registration whose runtime
// target ID another Runtime Broker row still holds. Like the name conflict,
// it carries only what the caller sent, never the other row's ID.
func runtimeTargetInUseRefusal(targetID string) *RuntimeTargetRefusal {
	return &RuntimeTargetRefusal{
		Code:   ErrCodeRuntimeTargetInUse,
		Status: http.StatusConflict,
		Message: fmt.Sprintf("runtime target %q already belongs to another Runtime Broker; a runtime target is registered by one Runtime Broker at a time (delete that Runtime Broker first, or register another target)",
			targetID),
		Details: map[string]interface{}{"runtimeTargetId": targetID},
	}
}

func runtimeTargetMismatchRefusal(m *api.RuntimeTargetMismatch) *RuntimeTargetRefusal {
	return &RuntimeTargetRefusal{
		Code:    ErrCodeRuntimeTargetMismatch,
		Status:  http.StatusConflict,
		Message: m.Message(),
		Details: m.Details(),
	}
}

func runtimeProfileUnsupportedRefusal(brokerID, profile string) *RuntimeTargetRefusal {
	return &RuntimeTargetRefusal{
		Code:   ErrCodeRuntimeProfileUnsupported,
		Status: http.StatusUnprocessableEntity,
		Message: fmt.Sprintf("Runtime Broker %s serves a single runtime target and does not accept Runtime Broker Profile %q",
			brokerID, profile),
		Details: map[string]interface{}{"runtimeBrokerId": brokerID, "profile": profile},
	}
}

func runtimeBrokerNotLinkedRefusal(brokerID, projectID string) *RuntimeTargetRefusal {
	return &RuntimeTargetRefusal{
		Code:   ErrCodeRuntimeBrokerNotLinked,
		Status: http.StatusUnprocessableEntity,
		Message: fmt.Sprintf("Runtime Broker %s is not linked to project %s; link it explicitly (scion runtime-broker provide, or POST /api/v1/projects/{id}/providers) before creating agents on it",
			brokerID, projectID),
		Details: map[string]interface{}{"runtimeBrokerId": brokerID, "projectId": projectID},
	}
}

func runtimeBrokerLinkPathUnsupportedRefusal(brokerID string) *RuntimeTargetRefusal {
	return &RuntimeTargetRefusal{
		Code:    ErrCodeRuntimeBrokerLinkPathUnsupported,
		Status:  http.StatusConflict,
		Message: "flat Runtime Brokers are linked only through POST /api/v1/projects/{id}/providers",
		Details: map[string]interface{}{"runtimeBrokerId": brokerID},
	}
}

// writeRuntimeTargetRefusal writes err as its own status, code and details
// when it is (or wraps) a *RuntimeTargetRefusal, and reports whether it did.
func writeRuntimeTargetRefusal(w http.ResponseWriter, err error) bool {
	var refusal *RuntimeTargetRefusal
	if !errors.As(err, &refusal) {
		return false
	}
	writeError(w, refusal.Status, refusal.Code, refusal.Message, refusal.Details)
	return true
}

// sameRuntimeTarget reports whether two descriptors name the same target
// (ID and type; the display name is not identity).
func sameRuntimeTarget(a, b *api.RuntimeTargetDescriptor) bool {
	return a != nil && b != nil && a.ID == b.ID && a.Type == b.Type
}

// copyRuntimeTarget returns a copy of d (nil for nil).
func copyRuntimeTarget(d *api.RuntimeTargetDescriptor) *api.RuntimeTargetDescriptor {
	if d == nil {
		return nil
	}
	c := *d
	return &c
}

// legacyRegistrationNameConflict refuses a legacy writer that would create a
// Runtime Broker row whose name or slug collides with a flat row (R4).
func legacyRegistrationNameConflict(ctx context.Context, s store.Store, name, slug, excludeID string) error {
	conflict, err := store.RuntimeBrokerNameConflict(ctx, s, name, slug, excludeID, true)
	if err != nil {
		return fmt.Errorf("failed to check Runtime Broker name conflicts: %w", err)
	}
	if conflict != nil {
		return runtimeBrokerNameConflictRefusal(name, slug)
	}
	return nil
}

// --- registration --------------------------------------------------------

// errFlatRegistrationIncomplete: a flat registration lacks its Runtime Broker
// ID or runtime target ID/type.
var errFlatRegistrationIncomplete = errors.New("a flat Runtime Broker registration requires brokerId, runtimeTarget.id and runtimeTarget.type")

// flatRegistration is one flat Runtime Broker registration, already admitted
// by the caller's registration authorization.
type flatRegistration struct {
	BrokerID  string
	Name      string
	Target    api.RuntimeTargetDescriptor
	CreatedBy string
	// Existing is the row the caller authorized against: the row with
	// BrokerID, matched by ID only (nil when none existed). The write is
	// pinned to it; a different answer at write time is
	// ErrBrokerRegistrationAuthorizationStale.
	Existing *store.RuntimeBroker
	// Apply sets the caller's non-identity metadata on the row being
	// created or re-registered (auto-provide, labels, endpoint,
	// capabilities). It never sets the target, name, slug or profiles.
	Apply func(b *store.RuntimeBroker, created bool)
	// GateAutoProvide applies the registration's auto-provide authorization
	// at the write, as the legacy registration does: a request turning
	// auto-provide on (RequestedAutoProvide) for a row that does not have
	// it needs AutoProvideAuthorized (broker.auto_provide, checked by the
	// caller); keeping it on, or turning it off, needs nothing extra. When
	// the row's auto-provide changed since that check, the write is refused
	// (ErrBrokerRegistrationAuthorizationStale) before anything is
	// written. The embedded registration does not set it: its auto-provide
	// comes from the co-located process's operator configuration.
	GateAutoProvide       bool
	RequestedAutoProvide  bool
	AutoProvideAuthorized bool
	// HostSAClaim is the caller's actAs decision for the host service
	// account Apply sets (Server.evaluateHostSAClaim). A row whose host
	// service account Apply changes to a non-empty value is written only
	// when HostSAClaim allows exactly that value; otherwise the registration
	// is refused with ErrHostSAClaimNotAuthorized before anything is
	// written. The zero value allows only writes that leave it unchanged,
	// which is all the embedded registration does.
	HostSAClaim hostSAClaimVerdict
}

// refuseRuntimeTargetInUse returns a runtime_target_in_use refusal when a
// Runtime Broker row other than brokerID holds targetID, and nil otherwise.
// A failed lookup is returned as an error.
func (s *Server) refuseRuntimeTargetInUse(ctx context.Context, targetID, brokerID string) error {
	holder, err := store.RuntimeTargetHolder(ctx, s.store, targetID, brokerID)
	if err != nil {
		return fmt.Errorf("failed to check runtime target %s: %w", targetID, err)
	}
	if holder != nil {
		return runtimeTargetInUseRefusal(targetID)
	}
	return nil
}

// registerFlatRuntimeBroker is the single Hub implementation of flat
// Runtime Broker registration (rules R1-R3, R5): the HTTP registration and
// the embedded registration both go through it. It returns the stored row
// and whether it was created. A refusal is a *RuntimeTargetRefusal and
// leaves every row unchanged. It never runs orphan reassignment and never
// writes profiles, links or project defaults.
func (s *Server) registerFlatRuntimeBroker(ctx context.Context, reg flatRegistration) (*store.RuntimeBroker, bool, error) {
	if reg.BrokerID == "" || reg.Target.ID == "" || reg.Target.Type == "" {
		return nil, false, errFlatRegistrationIncomplete
	}
	current, err := s.store.GetRuntimeBroker(ctx, reg.BrokerID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, false, fmt.Errorf("failed to read Runtime Broker %s: %w", reg.BrokerID, err)
	}
	if (reg.Existing == nil) != (current == nil) || (current != nil && current.ID != reg.Existing.ID) {
		return nil, false, ErrBrokerRegistrationAuthorizationStale
	}
	if reg.GateAutoProvide && reg.RequestedAutoProvide && !reg.AutoProvideAuthorized && (current == nil || !current.AutoProvide) {
		return nil, false, ErrBrokerRegistrationAuthorizationStale
	}

	if current == nil {
		// R1: a new flat registration needs the experiment.
		if !s.experimentEnabled(experiments.FlatRuntimeBrokers) {
			return nil, false, experimentDisabledRefusal()
		}
		// R2: the name/slug collision check applies when a flat
		// registration creates a row. It is check-then-act; the identity
		// file lock serializes a single host.
		slug := slugify(reg.Name)
		conflict, err := store.RuntimeBrokerNameConflict(ctx, s.store, reg.Name, slug, reg.BrokerID, false)
		if err != nil {
			return nil, false, fmt.Errorf("failed to check Runtime Broker name conflicts: %w", err)
		}
		if conflict != nil {
			return nil, false, runtimeBrokerNameConflictRefusal(reg.Name, slug)
		}
		// The target ID is unique across rows (the store's unique index):
		// another live row that holds it gets a clean refusal, not a
		// store error.
		if err := s.refuseRuntimeTargetInUse(ctx, reg.Target.ID, reg.BrokerID); err != nil {
			return nil, false, err
		}
		// R3: the target is written only here, at creation.
		now := time.Now()
		target := reg.Target
		b := &store.RuntimeBroker{
			ID:            reg.BrokerID,
			Name:          reg.Name,
			Slug:          slug,
			Status:        store.BrokerStatusOffline,
			Created:       now,
			Updated:       now,
			CreatedBy:     reg.CreatedBy,
			RuntimeTarget: &target,
		}
		if reg.Apply != nil {
			reg.Apply(b, true)
		}
		b.ID, b.Name, b.Slug, b.RuntimeTarget = reg.BrokerID, reg.Name, slug, &target
		b.Profiles, b.DefaultProfile = nil, ""
		if err := hostSAClaimWriteAllowed(hostSAClaim{}, hostSAClaimOf(b), reg.HostSAClaim); err != nil {
			return nil, false, err
		}
		if err := s.store.CreateRuntimeBrokerClearingCredentials(ctx, b); err != nil {
			// A row that took the target after the check above.
			var refusal *RuntimeTargetRefusal
			if errors.Is(err, store.ErrAlreadyExists) && errors.As(s.refuseRuntimeTargetInUse(ctx, reg.Target.ID, reg.BrokerID), &refusal) {
				return nil, false, refusal
			}
			return nil, false, fmt.Errorf("failed to create flat Runtime Broker: %w", err)
		}
		stored, err := s.store.GetRuntimeBroker(ctx, reg.BrokerID)
		if err != nil {
			return nil, false, fmt.Errorf("failed to read flat Runtime Broker: %w", err)
		}
		// A row created (or re-created after a deregistration) here gets its
		// agents' reservations back before the registration returns.
		s.BackfillBrokerOnRegistration(ctx, reg.BrokerID)
		return stored, true, nil
	}

	// R3: an existing legacy row is never converted, and a flat row's
	// target never changes. R1: an existing flat row with the same target
	// re-registers whatever the experiment state, so its agents are not
	// stranded.
	if !current.IsFlat() {
		return nil, false, runtimeBrokerNotFlatRefusal(current.ID)
	}
	if !sameRuntimeTarget(current.RuntimeTarget, &reg.Target) {
		return nil, false, runtimeTargetChangedRefusal(current.ID, current.RuntimeTarget.ID, reg.Target.ID)
	}
	// The host service account decision is checked before the first write
	// (the display name below), on a copy with Apply's changes, so a refusal
	// leaves the row unchanged. It is checked again on the row itself
	// immediately before the update.
	priorHostSA := hostSAClaimOf(current)
	if reg.Apply != nil {
		probe := *current
		probe.Labels = maps.Clone(current.Labels)
		reg.Apply(&probe, false)
		if err := hostSAClaimWriteAllowed(priorHostSA, hostSAClaimOf(&probe), reg.HostSAClaim); err != nil {
			return nil, false, err
		}
	}
	// R2: name and slug are set only at creation; a re-registration matched
	// by ID is never refused for a name collision.
	if !strings.EqualFold(current.Name, reg.Name) {
		slog.Warn("flat Runtime Broker re-registered with a different name; the stored name is kept (rename through the admin API)",
			"runtime_broker_id", current.ID, "stored_name", current.Name, "requested_name", reg.Name)
	}
	if conflict, err := store.RuntimeBrokerNameConflict(ctx, s.store, current.Name, current.Slug, current.ID, false); err == nil && conflict != nil {
		slog.Warn("flat Runtime Broker shares its name or slug with another Runtime Broker row",
			"runtime_broker_id", current.ID, "name", current.Name, "slug", current.Slug)
	}
	if reg.Target.DisplayName != current.RuntimeTarget.DisplayName {
		if _, err := s.store.SetRuntimeBrokerTarget(ctx, current.ID, reg.Target); err != nil {
			return nil, false, fmt.Errorf("failed to update the runtime target display name: %w", err)
		}
		if current, err = s.store.GetRuntimeBroker(ctx, current.ID); err != nil {
			return nil, false, fmt.Errorf("failed to read flat Runtime Broker: %w", err)
		}
	}
	if reg.Apply != nil {
		reg.Apply(current, false)
		current.Updated = time.Now()
		if err := hostSAClaimWriteAllowed(priorHostSA, hostSAClaimOf(current), reg.HostSAClaim); err != nil {
			return nil, false, err
		}
		if err := s.store.UpdateRuntimeBroker(ctx, current); err != nil {
			return nil, false, fmt.Errorf("failed to update flat Runtime Broker: %w", err)
		}
	}
	stored, err := s.store.GetRuntimeBroker(ctx, current.ID)
	if err != nil {
		return nil, false, fmt.Errorf("failed to read flat Runtime Broker: %w", err)
	}
	return stored, false, nil
}

// EmbeddedFlatRegistrationOptions carries the co-located process's
// non-identity metadata for the embedded flat registration.
type EmbeddedFlatRegistrationOptions struct {
	// Endpoint is the in-process Runtime Broker's HTTP endpoint.
	Endpoint string
	// AutoProvide is stored as sent; it never creates a link to a flat row.
	AutoProvide bool
	// Capabilities are the instance runtime's capabilities.
	Capabilities *store.BrokerCapabilities
	// WorkspaceStorage is the workspace storage descriptor (nil keeps the
	// stored one).
	WorkspaceStorage *api.BrokerWorkspaceStorage
	// HostIdentity is the host's GCP identity as read from the metadata
	// server; nil records nothing. It is recorded as an observation only
	// and never written to the GCP host fields.
	HostIdentity *store.HostIdentityObservation
}

// embeddedGlobalProjectSlug is the slug of the Hub's global project.
const embeddedGlobalProjectSlug = "global"

// RegisterEmbeddedFlatRuntimeBroker is the embedded (co-located) flat
// Runtime Broker registration (R7, R10). It applies the shared registration
// rules in-process and returns the stored row. The instance is activated only
// on a bound result: the stored row's ID and runtime target must match the
// identity (brokeridentity.CheckActivationAck, phase "embedded"). On success
// it records the row as the embedded Runtime Broker; every refusal is
// reported through EmbeddedBrokerRegistrationFailed and never falls back to
// a legacy identity. It creates the global project if it is missing, but
// never a provider link or project default for the flat row.
func (s *Server) RegisterEmbeddedFlatRuntimeBroker(ctx context.Context, id *brokeridentity.Identity, inst config.V1RuntimeBrokerInstanceConfig, opts ...EmbeddedFlatRegistrationOptions) (*store.RuntimeBroker, error) {
	row, err := s.registerEmbeddedFlat(ctx, id, inst, opts...)
	if err != nil {
		s.EmbeddedFlatInstanceFailed(inst.Key, err)
		return nil, err
	}
	s.mu.Lock()
	s.recordEmbeddedFlatActivatedLocked(inst.Key, row.ID)
	s.mu.Unlock()
	return row, nil
}

func (s *Server) registerEmbeddedFlat(ctx context.Context, id *brokeridentity.Identity, inst config.V1RuntimeBrokerInstanceConfig, opts ...EmbeddedFlatRegistrationOptions) (*store.RuntimeBroker, error) {
	if id == nil {
		return nil, errors.New("embedded flat Runtime Broker registration requires an identity")
	}
	var o EmbeddedFlatRegistrationOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	if err := s.ensureEmbeddedGlobalProject(ctx); err != nil {
		return nil, err
	}
	existing, err := s.store.GetRuntimeBroker(ctx, id.RuntimeBrokerID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("failed to read Runtime Broker %s: %w", id.RuntimeBrokerID, err)
	}
	target := id.RuntimeTarget
	if inst.RuntimeTarget != nil {
		target.DisplayName = inst.RuntimeTarget.DisplayName
	}
	if target.DisplayName == "" {
		target.DisplayName = target.Type
	}
	row, _, err := s.registerFlatRuntimeBroker(ctx, flatRegistration{
		BrokerID: id.RuntimeBrokerID,
		Name:     inst.Name,
		Target:   target,
		Existing: existing,
		Apply: func(b *store.RuntimeBroker, created bool) {
			b.Status = store.BrokerStatusOnline
			b.ConnectionState = "connected"
			b.Endpoint = o.Endpoint
			b.AutoProvide = o.AutoProvide
			b.LastHeartbeat = time.Now()
			// The embedded broker runs in the hub binary, so its version is
			// the hub's build token, written on every registration.
			b.Version = hubBuildToken()
			if o.Capabilities != nil {
				caps := *o.Capabilities
				b.Capabilities = &caps
			}
			if o.WorkspaceStorage != nil {
				b.WorkspaceStorage = o.WorkspaceStorage
			}
			if b.Labels == nil {
				b.Labels = map[string]string{}
			}
			b.Labels[brokerownership.LabelBrokerRole] = brokerownership.BrokerRoleEmbedded
		},
	})
	if err != nil {
		return nil, err
	}
	// R10: activation only on a bound result.
	if err := brokeridentity.CheckActivationAck(id, "embedded", row.ID, row.RuntimeTarget); err != nil {
		return nil, err
	}
	if o.HostIdentity != nil {
		s.recordHostObservation(ctx, row.ID, *o.HostIdentity)
		if stored, err := s.store.GetRuntimeBroker(ctx, row.ID); err == nil {
			row = stored
		}
	}
	return row, nil
}

// ensureEmbeddedGlobalProject creates the Hub's global project if it is
// missing, as the legacy embedded registration does. It sets no default
// Runtime Broker and writes no provider link.
func (s *Server) ensureEmbeddedGlobalProject(ctx context.Context) error {
	_, err := s.store.GetProjectBySlug(ctx, embeddedGlobalProjectSlug)
	if err == nil {
		return nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("failed to check for global project: %w", err)
	}
	p := &store.Project{
		ID:   api.NewUUID(),
		Name: "Global",
		Slug: embeddedGlobalProjectSlug,
		Labels: map[string]string{
			store.LabelSystemProject: "true",
			store.LabelGlobalProject: "true",
		},
	}
	if err := s.store.CreateProject(ctx, p); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		return fmt.Errorf("failed to create global project: %w", err)
	}
	return nil
}

// --- create placement ----------------------------------------------------

// flatCreatePlacement applies the new-create checks for a create on broker
// (nil when no Runtime Broker was resolved) and returns the placement to set
// on the agent: the pin for a flat row, the zero value for a legacy row. A
// refusal is a *RuntimeTargetRefusal, in the frozen precedence: experiment
// (412), expected target (409), explicit profile (422). Interactive creates,
// scheduled creates and the first placement of an existing agent with no
// Runtime Broker all call it, so the create paths cannot drift. It is
// evaluated after dispatch authorization and writes nothing.
func (s *Server) flatCreatePlacement(ctx context.Context, broker *store.RuntimeBroker, explicitProfile, expectedRuntimeTargetID string) (store.PinnedPlacement, error) {
	_ = ctx
	if !broker.IsFlat() {
		if expectedRuntimeTargetID == "" {
			return store.PinnedPlacement{}, nil
		}
		if !s.experimentEnabled(experiments.FlatRuntimeBrokers) {
			return store.PinnedPlacement{}, experimentDisabledRefusal()
		}
		brokerID := ""
		if broker != nil {
			brokerID = broker.ID
		}
		return store.PinnedPlacement{}, runtimeTargetMismatchRefusal(api.CheckExpectedRuntimeTarget(brokerID, "", expectedRuntimeTargetID))
	}
	if !s.experimentEnabled(experiments.FlatRuntimeBrokers) {
		return store.PinnedPlacement{}, experimentDisabledRefusal()
	}
	if m := api.CheckExpectedRuntimeTarget(broker.ID, broker.RuntimeTarget.ID, expectedRuntimeTargetID); m != nil {
		return store.PinnedPlacement{}, runtimeTargetMismatchRefusal(m)
	}
	if explicitProfile != "" {
		return store.PinnedPlacement{}, runtimeProfileUnsupportedRefusal(broker.ID, explicitProfile)
	}
	return store.PinnedPlacement{
		RuntimeBrokerID:   broker.ID,
		RuntimeTargetID:   broker.RuntimeTarget.ID,
		RuntimeTargetType: broker.RuntimeTarget.Type,
	}, nil
}

// applyPinnedPlacement sets placement p on a not-yet-stored agent model, so
// CreateAgent writes the pin in the same transaction as the row.
func applyPinnedPlacement(a *store.Agent, p store.PinnedPlacement) {
	if p.RuntimeTargetID == "" {
		return
	}
	a.RuntimeBrokerID = p.RuntimeBrokerID
	a.PinnedRuntimeBrokerID = p.RuntimeBrokerID
	a.PinnedRuntimeTargetID = p.RuntimeTargetID
	a.PinnedRuntimeTargetType = p.RuntimeTargetType
}

// flatDefaultProfileDroppedWarning is the dispatch warning for a default
// Runtime Broker Profile that was not applied because the target is flat.
func flatDefaultProfileDroppedWarning(profile, brokerID string) string {
	return fmt.Sprintf("default Runtime Broker Profile %q was not applied: Runtime Broker %s serves a single runtime target", profile, brokerID)
}

// --- pinned placement (lifecycle) ----------------------------------------

// ErrCodeRuntimeBrokerMissing is the 409 for a pinned agent whose Runtime
// Broker row no longer exists. Hub-only, like the other placement codes.
const ErrCodeRuntimeBrokerMissing = "runtime_broker_missing"

// inconsistentPlacementDocsURL is the docs anchor every placement refusal
// cites: what an inconsistent saved placement means and what an operator
// can (and cannot) do about it.
const inconsistentPlacementDocsURL = "https://googlecloudplatform.github.io/scion/" +
	"hosted/ha/multi-broker/#inconsistent-agent-placement"

// inconsistentPlacementMessage is the frozen guidance text of a placement
// refusal. It never suggests deleting and recreating the agent or a
// project force delete: an ordinary delete refuses the same agent.
func inconsistentPlacementMessage(a *store.Agent, reason string) string {
	return fmt.Sprintf("agent %s is pinned to Runtime Broker %q (target %q) but its saved placement is inconsistent (%s); "+
		"Scion will not operate on it through another Runtime Broker. "+
		"No per-agent repair is available in this release (tracked in ptone/scion#4844); see %s.",
		a.ID, a.PinnedRuntimeBrokerID, a.PinnedRuntimeTargetID, reason, inconsistentPlacementDocsURL)
}

// placementRefusalDetails are the details of a placement refusal: the
// agent, its pin and the Runtime Broker it is assigned to.
func placementRefusalDetails(a *store.Agent) map[string]interface{} {
	return map[string]interface{}{
		"agentId":               a.ID,
		"pinnedRuntimeBrokerId": a.PinnedRuntimeBrokerID,
		"pinnedRuntimeTargetId": a.PinnedRuntimeTargetID,
		"runtimeBrokerId":       a.RuntimeBrokerID,
	}
}

// stalePinRefusal is the 409 runtime_target_pin_stale for agent a whose
// saved placement is inconsistent for reason.
func stalePinRefusal(a *store.Agent, reason string) *RuntimeTargetRefusal {
	return &RuntimeTargetRefusal{
		Code:    ErrCodeRuntimeTargetPinStale,
		Status:  http.StatusConflict,
		Message: inconsistentPlacementMessage(a, reason),
		Details: placementRefusalDetails(a),
	}
}

// runtimeBrokerMissingRefusal is the 409 runtime_broker_missing for a
// pinned agent whose Runtime Broker row is gone.
func runtimeBrokerMissingRefusal(a *store.Agent) *RuntimeTargetRefusal {
	return &RuntimeTargetRefusal{
		Code:   ErrCodeRuntimeBrokerMissing,
		Status: http.StatusConflict,
		Message: inconsistentPlacementMessage(a,
			fmt.Sprintf("the Runtime Broker this agent is pinned to, %s, is no longer registered", a.RuntimeBrokerID)),
		Details: placementRefusalDetails(a),
	}
}

// PlacementConflictError is a refusal by requirePinnedPlacement: the
// agent's saved placement does not allow a broker-reaching operation.
// Nothing was dispatched and nothing was written. It wraps the
// *RuntimeTargetRefusal that handlers write with its own status, code and
// details (writeRuntimeTargetRefusal), and it answers HTTPStatus so a
// handler that writes errors with writeErrorFromErr gets the same 409.
//
// The delete engine treats it like a run mismatch (refuseDeletePlacement):
// a delete never finalizes from it, under force or best-effort too.
type PlacementConflictError struct {
	Refusal *RuntimeTargetRefusal
}

func (e *PlacementConflictError) Error() string { return e.Refusal.Error() }

func (e *PlacementConflictError) Unwrap() error { return e.Refusal }

// HTTPStatus implements the apierr status interface.
func (e *PlacementConflictError) HTTPStatus() (int, string, string) {
	return e.Refusal.Status, e.Refusal.Code, e.Refusal.Message
}

func placementConflict(r *RuntimeTargetRefusal) *PlacementConflictError {
	return &PlacementConflictError{Refusal: r}
}

// placementRefusalCodes are the codes requirePinnedPlacement refuses with.
var placementRefusalCodes = map[string]bool{
	ErrCodeRuntimeTargetPinStale: true,
	ErrCodeRuntimeBrokerMissing:  true,
	ErrCodeRuntimeTargetMismatch: true,
}

// placementRefusalOf returns the placement refusal err carries: a
// *PlacementConflictError raised on this node, or a *RuntimeTargetRefusal
// with a placement code rebuilt from a cross-node dispatch row (which
// travels as a plain refusal, dispatchFailureError). Either way the Hub
// refused before anything was sent.
func placementRefusalOf(err error) (*RuntimeTargetRefusal, bool) {
	var pc *PlacementConflictError
	if errors.As(err, &pc) && pc.Refusal != nil {
		return pc.Refusal, true
	}
	var r *RuntimeTargetRefusal
	if errors.As(err, &r) && r != nil && placementRefusalCodes[r.Code] {
		return r, true
	}
	return nil, false
}

// requirePinnedPlacement is the complete saved-placement check for a
// broker-reaching operation on an existing agent. It runs after the
// operation's authorization and before any dispatch, claim, intent,
// reservation, credential or run-ID write, and it only reads: the agent
// and its Runtime Broker row.
//
//   - unpinned, no Runtime Broker: errNoRuntimeBrokerAssigned (today's
//     requireRuntimeBrokerAssigned answer);
//   - unpinned, Runtime Broker row missing: nil (today's handling applies);
//   - unpinned, legacy row: nil (the legacy path is unchanged);
//   - unpinned, flat row: 409 runtime_target_pin_stale;
//   - pinned with an incomplete pin, no Runtime Broker, a pin naming
//     another Runtime Broker, or a row that is not flat: 409
//     runtime_target_pin_stale;
//   - pinned, Runtime Broker row missing: 409 runtime_broker_missing;
//   - pinned, the row serves another runtime target: 409
//     runtime_target_mismatch;
//   - pinned and consistent: nil, with the row.
//
// Every refusal is a *PlacementConflictError. A store failure is returned
// as is (a 500 for the caller, with nothing dispatched).
func requirePinnedPlacement(ctx context.Context, st store.Store, a *store.Agent) (*store.RuntimeBroker, error) {
	if a == nil {
		return nil, errNoRuntimeBrokerAssigned
	}
	if !a.IsPinned() {
		if a.RuntimeBrokerID == "" {
			return nil, errNoRuntimeBrokerAssigned
		}
		broker, err := st.GetRuntimeBroker(ctx, a.RuntimeBrokerID)
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if broker.IsFlat() {
			return broker, placementConflict(stalePinRefusal(a,
				fmt.Sprintf("it has no pin, but its Runtime Broker %s serves a single runtime target", a.RuntimeBrokerID)))
		}
		return broker, nil
	}
	if a.PinnedRuntimeBrokerID == "" || a.PinnedRuntimeTargetType == "" {
		return nil, placementConflict(stalePinRefusal(a, "the pin is incomplete"))
	}
	if a.RuntimeBrokerID == "" {
		return nil, placementConflict(stalePinRefusal(a, "it has no Runtime Broker assigned"))
	}
	if !a.PinValid() {
		return nil, placementConflict(stalePinRefusal(a,
			fmt.Sprintf("it is assigned to Runtime Broker %s", a.RuntimeBrokerID)))
	}
	broker, err := st.GetRuntimeBroker(ctx, a.RuntimeBrokerID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, placementConflict(runtimeBrokerMissingRefusal(a))
	}
	if err != nil {
		return nil, err
	}
	if !broker.IsFlat() {
		return broker, placementConflict(stalePinRefusal(a,
			fmt.Sprintf("Runtime Broker %s does not serve a single runtime target", a.RuntimeBrokerID)))
	}
	if broker.RuntimeTarget.ID != a.PinnedRuntimeTargetID {
		return broker, placementConflict(runtimeTargetMismatchRefusal(
			api.CheckExpectedRuntimeTarget(broker.ID, broker.RuntimeTarget.ID, a.PinnedRuntimeTargetID)))
	}
	return broker, nil
}

// placementBackstop is the dispatcher's saved-placement check
// (requirePinnedPlacement) for every broker-reaching method of an existing
// agent. It runs inside the dispatcher method, so it also covers a caller
// with no handler pre-check and a queued dispatch executed on the node that
// holds the broker connection.
//
// deletePreviousRuns has no backstop of its own: its one caller,
// DispatchAgentDelete, already ran this one, and a second check after the
// current run's delete succeeded would read as "nothing was dispatched"
// and roll back a delete whose current run is gone.
func (d *HTTPAgentDispatcher) placementBackstop(ctx context.Context, agent *store.Agent) error {
	if d.store == nil {
		return errPlacementCheckNoStore
	}
	_, err := requirePinnedPlacement(ctx, d.store, agent)
	return err
}

// errPlacementCheckNoStore fails a dispatcher with no store closed: the
// placement cannot be checked, so nothing is dispatched.
var errPlacementCheckNoStore = errors.New("placement check: the dispatcher has no store")

// keysPlacementBackstop is placementBackstop for DispatchAgentKeys, whose
// target is not an agent: it loads the agent row, checks it, and requires
// target to name the agent's current Runtime Broker.
func (d *HTTPAgentDispatcher) keysPlacementBackstop(ctx context.Context, target agentkeys.Target) error {
	if d.store == nil {
		return errPlacementCheckNoStore
	}
	agent, err := d.store.GetAgent(ctx, target.AgentID)
	if err != nil {
		return fmt.Errorf("load agent %s for the placement check: %w", target.AgentID, err)
	}
	if _, err := requirePinnedPlacement(ctx, d.store, agent); err != nil {
		return err
	}
	if target.RuntimeBrokerID != agent.RuntimeBrokerID {
		return placementConflict(stalePinRefusal(agent,
			fmt.Sprintf("the request names Runtime Broker %s", target.RuntimeBrokerID)))
	}
	return nil
}

// checkPinnedPlacement is the handler pre-check form of
// requirePinnedPlacement (checkAgentPinnedPlacement on the Server's store).
func (s *Server) checkPinnedPlacement(agent *store.Agent) error {
	_, err := s.pinnedPlacementBroker(context.Background(), agent)
	return err
}

// pinnedPlacementBroker is checkPinnedPlacement returning the agent's
// Runtime Broker row (nil when it has none or an unpinned agent's row is
// missing).
func (s *Server) pinnedPlacementBroker(ctx context.Context, agent *store.Agent) (*store.RuntimeBroker, error) {
	return checkAgentPinnedPlacement(ctx, s.store, agent)
}

// checkAgentPinnedPlacement is requirePinnedPlacement for a caller that
// itself handles an agent with no Runtime Broker at all: an unpinned agent
// with no Runtime Broker passes (nil, nil), so that caller's existing
// handling applies (a stop records the stopped status, a start answers its
// own missing-Runtime-Broker error). Every other state is
// requirePinnedPlacement's answer, so a pinned agent with no Runtime Broker
// or a missing row is refused.
func checkAgentPinnedPlacement(ctx context.Context, st store.Store, agent *store.Agent) (*store.RuntimeBroker, error) {
	if st == nil || agent == nil || (!agent.IsPinned() && agent.RuntimeBrokerID == "") {
		return nil, nil
	}
	return requirePinnedPlacement(ctx, st, agent)
}

// profileDefaultSAUnlessPinned is projectProfileDefaultSA for the create
// paths: a pinned agent never gets a profile written onto it (the flat
// contract), so the per-profile default lookup is skipped for it. It reads
// the in-memory pin set before this point (applyPinnedPlacement), with no
// broker read, so it cannot fail open.
func (s *Server) profileDefaultSAUnlessPinned(ctx context.Context, agent *store.Agent, runtimeBrokerID string, project *store.Project) (profile, saID string) {
	if agent.IsPinned() {
		return "", ""
	}
	requestProfile := ""
	if agent.AppliedConfig != nil {
		requestProfile = agent.AppliedConfig.Profile
	}
	return s.projectProfileDefaultSA(ctx, runtimeBrokerID, project, requestProfile)
}

// lifecyclePlacementCheck applies the lifecycle-branch checks for resuming or
// starting an existing agent in place on its own Runtime Broker: stale pin
// (409), then a client-supplied expected target compared with the agent's
// valid pin (409), then an explicit profile toward a flat row (422). The
// experiment plays no part.
func (s *Server) lifecyclePlacementCheck(ctx context.Context, agent *store.Agent, explicitProfile, expectedRuntimeTargetID string) error {
	broker, err := s.pinnedPlacementBroker(ctx, agent)
	if err != nil {
		return err
	}
	if expectedRuntimeTargetID != "" {
		actual := ""
		if agent.PinValid() {
			actual = agent.PinnedRuntimeTargetID
		}
		if m := api.CheckExpectedRuntimeTarget(agent.RuntimeBrokerID, actual, expectedRuntimeTargetID); m != nil {
			return runtimeTargetMismatchRefusal(m)
		}
	}
	if explicitProfile != "" && broker.IsFlat() {
		return runtimeProfileUnsupportedRefusal(broker.ID, explicitProfile)
	}
	return nil
}

// validPinnedTarget returns the agent's pinned runtime target when its pin is
// valid, "" otherwise. Start and restart send it as expectedRuntimeTargetId.
func validPinnedTarget(a *store.Agent) string {
	if a.PinValid() {
		return a.PinnedRuntimeTargetID
	}
	return ""
}

// --- relay ---------------------------------------------------------------

// runtimeTargetRelayDetailKeys are the frozen details keys relayed for each
// flat wire code a Runtime Broker returns.
var runtimeTargetRelayDetailKeys = map[string][]string{
	ErrCodeRuntimeTargetMismatch:     {"runtimeBrokerId", "expectedRuntimeTargetId", "actualRuntimeTargetId"},
	ErrCodeRuntimeProfileUnsupported: {"runtimeBrokerId", "profile"},
	ErrCodeRuntimeTargetRequired:     {"runtimeBrokerId"},
}

// relayRuntimeTargetError writes a flat Runtime Broker refusal with its own
// status, code, message and frozen details, and reports whether it did: a
// Hub *RuntimeTargetRefusal, or a Runtime Broker 409/412/422 carrying one of
// the shared flat wire codes. Start markers are never relayed in a Hub
// public envelope. Any other error is left to the caller.
func relayRuntimeTargetError(w http.ResponseWriter, err error) bool {
	if writeRuntimeTargetRefusal(w, err) {
		return true
	}
	se, code, details, ok := brokerRuntimeTargetRefusal(err)
	if !ok {
		return false
	}
	writeError(w, se.StatusCode, code, se.brokerErrorMessage(), details)
	return true
}

// runtimeTargetRelayStatus is the HTTP status each shared flat wire code is
// returned with by a Runtime Broker.
var runtimeTargetRelayStatus = map[string]int{
	ErrCodeRuntimeTargetMismatch:     http.StatusConflict,
	ErrCodeRuntimeProfileUnsupported: http.StatusUnprocessableEntity,
	ErrCodeRuntimeTargetRequired:     http.StatusPreconditionFailed,
}

// brokerRuntimeTargetRefusal reports whether err is a Runtime Broker answer
// carrying one of the shared flat wire codes with its own status (409, 422
// or 412), and returns it with its code and its frozen details (start
// markers and any other key dropped).
func brokerRuntimeTargetRefusal(err error) (*brokerStatusError, string, map[string]interface{}, bool) {
	var se *brokerStatusError
	if !errors.As(err, &se) {
		return nil, "", nil, false
	}
	code := se.brokerErrorCode()
	keys, ok := runtimeTargetRelayDetailKeys[code]
	if !ok || runtimeTargetRelayStatus[code] != se.StatusCode {
		return nil, "", nil, false
	}
	brokerDetails := se.brokerErrorDetails()
	details := make(map[string]interface{}, len(keys))
	for _, k := range keys {
		if v, ok := brokerDetails[k]; ok {
			details[k] = v
		}
	}
	return se, code, details, true
}

// relayDispatchRefusal relays the Runtime Broker answers a create, start or
// restart error switch passes through verbatim instead of mapping them to
// 502: flat Runtime Broker refusals and required-skill resolution failures.
func relayDispatchRefusal(w http.ResponseWriter, err error) bool {
	return relayRuntimeTargetError(w, err) || relaySkillResolutionError(w, err)
}

// runtimeTargetDMErrorIfAny maps a flat Runtime Broker refusal (a Hub
// *RuntimeTargetRefusal, or a Runtime Broker 409/412/422 with a shared flat
// code) to the wake path's AgentDMError with its own status, code and
// details; nil for any other error.
func runtimeTargetDMErrorIfAny(err error) *AgentDMError {
	var refusal *RuntimeTargetRefusal
	if errors.As(err, &refusal) {
		return &AgentDMError{Code: refusal.Code, Message: refusal.Message, HTTPStatus: refusal.Status, Details: refusal.Details}
	}
	if se, code, details, ok := brokerRuntimeTargetRefusal(err); ok {
		return &AgentDMError{Code: code, Message: se.brokerErrorMessage(), HTTPStatus: se.StatusCode, Details: details}
	}
	return nil
}

// runtimeTargetDMError is runtimeTargetDMErrorIfAny for a pre-check error,
// falling back to an internal error for anything else.
func runtimeTargetDMError(err error) *AgentDMError {
	if dmErr := runtimeTargetDMErrorIfAny(err); dmErr != nil {
		return dmErr
	}
	return &AgentDMError{Code: ErrCodeInternalError, Message: "Failed to wake agent: " + err.Error(), HTTPStatus: http.StatusInternalServerError}
}

// runtimeTargetRefusalMessage returns the message of a flat Runtime Broker
// refusal returned by a start (the hub's own pre-check refusal, or the
// broker's), or "" for any other error.
func runtimeTargetRefusalMessage(err error) string {
	var refusal *RuntimeTargetRefusal
	if errors.As(err, &refusal) {
		return refusal.Message
	}
	if se, _, _, ok := brokerRuntimeTargetRefusal(err); ok {
		return se.brokerErrorMessage()
	}
	return ""
}

// settleRuntimeTargetRefusal records a flat Runtime Broker refusal returned
// by a start as a definite start failure: the agent's message is set to the
// refusal message. Nothing retries a refusal. Any other error is ignored.
func (s *Server) settleRuntimeTargetRefusal(ctx context.Context, agent *store.Agent, err error) {
	msg := runtimeTargetRefusalMessage(err)
	if msg == "" || agent == nil {
		return
	}
	if uerr := s.store.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{Message: msg}); uerr != nil {
		slog.Warn("failed to record a flat Runtime Broker start refusal on the agent", "agent_id", agent.ID, "error", uerr)
		return
	}
	agent.Message = msg
}
