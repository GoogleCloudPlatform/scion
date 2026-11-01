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

package lifecycleevents

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

// ProducerNamespacePrefix prefixes the hub ID in producer_namespace.
const ProducerNamespacePrefix = "scion-hub/"

// Creator is the create-time provenance the cursor records at created and
// later snapshots copy: the creating principal and the requested template
// name.
type Creator struct {
	Type string
	ID   string
	// RequestedTemplate is the requested template name, known only from
	// the create itself or the cursor it created; "" (omitted) otherwise,
	// never agents.template, which a broker echo rewrites (WP1 slice 3
	// addendum v12 section 4a).
	RequestedTemplate string
}

// Snapshot returns the metadata snapshot of a (design 3.2). The effective
// values come from the stored broker echo (a.EffectiveConfig, whatever its
// run; buildEvent keeps them only on events of that run) and stay unknown
// (omitted) without one; requested values never substitute for them.
func Snapshot(a *store.Agent, creator Creator) telemetrycontract.LifecycleMetadata {
	m := telemetrycontract.LifecycleMetadata{
		Name:                 a.Name,
		Slug:                 a.Slug,
		RequestedTemplate:    creator.RequestedTemplate,
		Runtime:              a.Runtime,
		CreatorPrincipalType: creator.Type,
		CreatorPrincipalID:   creator.ID,
		Labels:               telemetrycontract.LabelsFromMap(a.Labels),
	}
	if c := a.AppliedConfig; c != nil {
		m.RequestedTemplateID = c.TemplateID
		m.RequestedTemplateHash = c.TemplateHash
		m.RequestedModel = c.Model
		m.RequestedModelAlias = c.ModelSize
		m.Harness = c.Harness
		// The name from applied_config, never the denormalized
		// harness_config column a boot reconcile rewrites (design 3.2).
		m.HarnessConfig = c.HarnessConfig
		m.HarnessConfigSource = c.HarnessConfigSource
		m.Profile = c.Profile
	}
	if e := a.EffectiveConfig; e != nil {
		m.EffectiveTemplate = e.Template
		m.EffectiveTemplateHash = e.TemplateHash
		m.EffectiveModel = e.Model
		m.EffectiveHarness = e.Harness
		m.HarnessConfigRevision = e.HarnessConfigRevision
		m.EffectiveBrokerBuild = e.BrokerBuild
	}
	// The last ancestor is the parent agent when the chain is longer than
	// the root principal alone.
	if n := len(a.Ancestry); n > 1 {
		m.ParentAgentID = a.Ancestry[n-1]
	}
	return m
}

// MetadataDigest is the SHA-256 of the snapshot's canonical JSON (struct
// fields in declaration order, labels bytewise sorted), so it is stable
// under map iteration order.
func MetadataDigest(m telemetrycontract.LifecycleMetadata) (string, error) {
	data, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("metadata digest: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// ConfigDigest is the SHA-256 of the snapshot's configuration fields only
// (requested template, model, harness, harness config, profile, runtime
// and the effective fields): the identity and user metadata (name, slug,
// labels, creator, parent) are cleared first. A config-resolution write
// that changes it emits configured (design 2.2, slice 2 ruling Q1).
func ConfigDigest(m telemetrycontract.LifecycleMetadata) (string, error) {
	m.Name, m.Slug, m.Labels = "", "", nil
	m.CreatorPrincipalType, m.CreatorPrincipalID, m.ParentAgentID = "", "", ""
	data, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("config digest: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// withEchoRun folds the stored echo's run into a digest (addendum v12
// section 4): a new run's first echo moves the digest even when its values
// equal the previous run's. With no echo (runID "") the digest is
// returned unchanged, so agents without one keep their digests.
func withEchoRun(digest, runID string) string {
	if runID == "" {
		return digest
	}
	sum := sha256.Sum256([]byte(digest + "\x00effective-run\x00" + runID))
	return hex.EncodeToString(sum[:])
}

// echoRunID is the run of a's stored echo, "" without one.
func echoRunID(a *store.Agent) string {
	if a.EffectiveConfig == nil {
		return ""
	}
	return a.EffectiveConfig.RunID
}

// keepEffectiveFor clears the effective fields of m unless the stored echo
// belongs to launchID, the event's final launch_id (addendum v12 section
// 4): an event of another run, or with no launch_id, never carries them.
func keepEffectiveFor(m *telemetrycontract.LifecycleMetadata, a *store.Agent, launchID string) {
	if launchID != "" && echoRunID(a) == launchID {
		return
	}
	m.EffectiveTemplate, m.EffectiveTemplateHash, m.EffectiveModel = "", "", ""
	m.EffectiveHarness, m.HarnessConfigRevision, m.EffectiveBrokerBuild = "", "", ""
}

// Fingerprint is the SHA-256 over the derive-relevant fields of a row
// (design section 4, B3): phase, launch state, the async launch ID column,
// generation, deleted_at and the metadata digest. The run ID is excluded
// on purpose: an unobserved writer changes it on every start. deleted_at
// is truncated to microseconds, the storage precision, so an in-memory row
// and its stored copy fingerprint alike.
func Fingerprint(a *store.Agent, digest string) string {
	deleted := ""
	if !a.DeletedAt.IsZero() {
		deleted = telemetrycontract.FormatTS(a.DeletedAt)
	}
	h := sha256.New()
	for _, part := range []string{
		"v1", a.Phase, a.LaunchState, a.LaunchID, strconv.Itoa(a.Generation), deleted, digest,
	} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// creatorOf returns the provenance to record for a row: the create context
// on a create, the cursor's on later writes, and for an agent with no
// cursor (pre-rollout) its created_by with the type unknown (design 3.2).
// atCreate marks the create write itself, whose committed row's template
// is the requested template (addendum v12 section 4a); otherwise it comes
// only from the cursor.
func creatorOf(a *store.Agent, cursor *store.LifecycleCursor, create *CreateContext, atCreate bool) Creator {
	var c Creator
	switch {
	case create != nil:
		c = Creator{Type: create.CreatorType, ID: create.CreatorID}
	case cursor != nil:
		c = Creator{Type: cursor.CreatorPrincipalType, ID: cursor.CreatorPrincipalID, RequestedTemplate: cursor.RequestedTemplate}
	default:
		c = Creator{ID: a.CreatedBy}
	}
	if atCreate {
		c.RequestedTemplate = a.Template
	}
	return c
}

// eventInput is everything needed to build one event.
type eventInput struct {
	Planned            Planned
	Row                *store.Agent
	TransitionRevision int64
	MetadataRevision   int64
	Metadata           *telemetrycontract.LifecycleMetadata // snapshot transitions only
	Cursor             *store.LifecycleCursor               // the locked cursor, nil if none
	EventID            string
	HubID              string
	ProducerBuild      string
	EventTS            time.Time
	ObservedTS         time.Time
}

// buildEvent assembles one event (design 3.2) and validates it.
func buildEvent(in eventInput) (*telemetrycontract.LifecycleEvent, error) {
	p, a := in.Planned, in.Row
	if a == nil {
		return nil, fmt.Errorf("build %s: no row", p.Transition)
	}
	ev := &telemetrycontract.LifecycleEvent{
		Envelope: telemetrycontract.NewEnvelope(telemetrycontract.EventTypeLifecycle, in.EventID,
			ProducerNamespacePrefix+in.HubID, in.EventTS, in.ObservedTS),
		Transition:         p.Transition,
		TransitionRevision: in.TransitionRevision,
		MetadataRevision:   in.MetadataRevision,
		EndReason:          p.EndReason,
		ErrorCode:          p.ErrorCode,
		Cause:              p.Cause,
		StopKind:           p.StopKind,
		DeleteMode:         p.DeleteMode,
		LaunchOwner:        p.LaunchOwner,
	}
	ev.HubID = in.HubID
	ev.ProjectID = a.ProjectID
	ev.AgentID = a.ID
	ev.BrokerID = a.RuntimeBrokerID
	ev.ProducerBuild = in.ProducerBuild
	gen := a.Generation
	if p.NextGeneration {
		gen++
	}
	if gen > 0 {
		ev.IncarnationID = strconv.Itoa(gen)
	}
	if !p.OmitLaunchID {
		ev.LaunchID = a.RunID
	}
	if p.AsyncLaunch {
		ev.AsyncLaunchID = a.LaunchID
	}
	if p.Gap {
		gap := true
		ev.Gap = &gap
	}
	if telemetrycontract.IsSnapshotTransition(p.Transition) {
		if in.Metadata == nil {
			return nil, fmt.Errorf("build %s: snapshot transition without metadata", p.Transition)
		}
		md := *in.Metadata
		ev.Metadata = &md
	}

	// A closure of an open segment carries that segment's frozen run
	// identity and metadata revision, taken from the cursor (review r6 N6:
	// presence is enforced here, where the cursor is known).
	if p.ClosesOpenRun {
		c := in.Cursor
		if c == nil || c.OpenRun == "" || c.OpenRunMetadataRevision < 1 {
			return nil, fmt.Errorf("build %s: closes an open run segment but the cursor has none", p.Transition)
		}
		seg := c.OpenRunMetadataRevision
		ev.SegmentMetadataRevision = &seg
		ev.LaunchID = c.OpenRun
		if c.OpenRunGeneration != "" {
			ev.IncarnationID = c.OpenRunGeneration
		}
		if p.StopKind == telemetrycontract.StopKindSuperseded && c.PendingRunChangeAt != nil {
			ev.RunChangedAt = telemetrycontract.FormatTS(*c.PendingRunChangeAt)
		}
	}
	// Attribution (design 4.3), after the closure override: a closure of
	// an untrusted segment, or an event whose run is unproven, omits
	// launch_id; an agent-state event may name the attributed run.
	if p.AttrOmitLaunch {
		ev.LaunchID = ""
		// An untrusted segment terminal keeps the segment's frozen
		// generation, an observed fact (design 4.3 rule 1), as a closure
		// of an open segment already does above.
		if p.ClosesSegment && !p.ClosesOpenRun && in.Cursor != nil && in.Cursor.OpenRunGeneration != "" {
			ev.IncarnationID = in.Cursor.OpenRunGeneration
		}
	} else if p.AttrLaunch != "" {
		ev.LaunchID = p.AttrLaunch
	}
	if p.CarriesAdoption && !p.DropAdoption && ev.LaunchID != "" {
		c := in.Cursor
		if c == nil || c.PendingAdoptedFrom == "" || c.PendingAdoptedAt == nil {
			return nil, fmt.Errorf("build %s: carries an adoption but the cursor has none", p.Transition)
		}
		ev.AdoptedFromLaunchID = c.PendingAdoptedFrom
		ev.AdoptedAt = telemetrycontract.FormatTS(*c.PendingAdoptedAt)
		ev.AdoptionSource = c.PendingAdoptionSource
	}
	// After the closure override: the filter reads the final launch_id.
	// The reduced block is this event's copy and is never digested.
	if ev.Metadata != nil {
		keepEffectiveFor(ev.Metadata, a, ev.LaunchID)
	}
	if err := ev.Validate(); err != nil {
		return nil, err
	}
	return ev, nil
}

// buildFailureReason reduces a build error to a low-cardinality reason.
func buildFailureReason(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "UTF-8"):
		return "invalid_utf8"
	case strings.Contains(msg, "label"):
		return "invalid_labels"
	case strings.Contains(msg, "open run segment"):
		return "segment_state"
	case strings.Contains(msg, "digest"):
		return "digest"
	default:
		return "invalid_event"
	}
}
