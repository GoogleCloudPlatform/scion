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

// Creator is the principal recorded as an agent's creator.
type Creator struct {
	Type string
	ID   string
}

// Snapshot returns the metadata snapshot of a (design 3.2). Effective
// values stay unknown (omitted) until the broker echoes them; requested
// values never substitute for them.
func Snapshot(a *store.Agent, creator Creator) telemetrycontract.LifecycleMetadata {
	m := telemetrycontract.LifecycleMetadata{
		Name:                 a.Name,
		Slug:                 a.Slug,
		RequestedTemplate:    a.Template,
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

// creatorOf returns the creator to record for a row: the create context on
// a create, the cursor's on later writes, and for an agent with no cursor
// (pre-rollout) its created_by with the type unknown (design 3.2).
func creatorOf(a *store.Agent, cursor *store.LifecycleCursor, create *CreateContext) Creator {
	switch {
	case create != nil:
		return Creator{Type: create.CreatorType, ID: create.CreatorID}
	case cursor != nil:
		return Creator{Type: cursor.CreatorPrincipalType, ID: cursor.CreatorPrincipalID}
	default:
		return Creator{ID: a.CreatedBy}
	}
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
	}
	ev.HubID = in.HubID
	ev.ProjectID = a.ProjectID
	ev.AgentID = a.ID
	ev.BrokerID = a.RuntimeBrokerID
	ev.ProducerBuild = in.ProducerBuild
	if a.Generation > 0 {
		ev.IncarnationID = strconv.Itoa(a.Generation)
	}
	ev.LaunchID = a.RunID
	if p.Transition == telemetrycontract.TransitionLaunchFailed {
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
