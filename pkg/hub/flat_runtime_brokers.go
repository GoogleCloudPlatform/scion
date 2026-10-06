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
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
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
		if err := s.store.CreateRuntimeBroker(ctx, b); err != nil {
			return nil, false, fmt.Errorf("failed to create flat Runtime Broker: %w", err)
		}
		stored, err := s.store.GetRuntimeBroker(ctx, reg.BrokerID)
		if err != nil {
			return nil, false, fmt.Errorf("failed to read flat Runtime Broker: %w", err)
		}
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
		s.EmbeddedBrokerRegistrationFailed(err)
		return nil, err
	}
	s.SetEmbeddedBrokerID(row.ID)
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
			if created {
				b.Version = "0.1.0"
			}
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
			b.Labels["scion.io/broker-role"] = "embedded"
		},
	})
	if err != nil {
		return nil, err
	}
	// R10: activation only on a bound result.
	if err := brokeridentity.CheckActivationAck(id, "embedded", row.ID, row.RuntimeTarget); err != nil {
		return nil, err
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
			"scion.io/system": "true",
			"scion.io/global": "true",
		},
	}
	if err := s.store.CreateProject(ctx, p); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		return fmt.Errorf("failed to create global project: %w", err)
	}
	return nil
}
