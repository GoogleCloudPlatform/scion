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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// A flat Runtime Broker instance publishes its FC4-a presentation
// (api.RuntimeTargetPresentation; .design/flat-runtime-brokers/contract-appendix-p3.md
// FC4/FC4-a, ptone/scion#3282) on every heartbeat; the Hub stores it
// read-only on the broker row. ApplyRuntimeTargetPresentation is the one
// writer: handleBrokerHeartbeat calls it after checking that the request is
// authenticated as the broker itself (write rule 1), and the co-located flat
// heartbeat loop (cmd/server_flat_host.go), which is in-process and
// trusted, calls it directly. It is a Server method, not a free function
// over the store, because write rule 2 needs the Hub's experiment state.

// Reasons ApplyRuntimeTargetPresentation ignores a presentation. An ignored
// presentation leaves the stored value unchanged.
const (
	presentationIgnoredOmitted       = "omitted"
	presentationIgnoredExperimentOff = "experiment_off"
	presentationIgnoredSchemaVersion = "unknown_schema_version"
	presentationIgnoredLegacyRow     = "legacy_row"
	presentationIgnoredTarget        = "target_mismatch"
)

// PresentationResult is what ApplyRuntimeTargetPresentation did: either the
// store outcome (written, confirmed or stale), or the reason the
// presentation was ignored before any write.
type PresentationResult struct {
	Outcome store.PresentationOutcome
	Ignored string
}

// RuntimeTargetPresentationHash is the Hub's hash of a presentation: the
// hex SHA-256 of the canonical JSON of its FC4-a fields (SchemaVersion is
// not one). encoding/json writes struct fields in declaration order and map
// keys sorted, so equal presentations hash equally. The Hub computes it over
// the presentation as stored (after presentationForStore); it never takes a hash from a Runtime Broker.
func RuntimeTargetPresentationHash(p *api.RuntimeTargetPresentation) (string, error) {
	if p == nil {
		return "", errors.New("nil presentation")
	}
	fields := *p
	fields.SchemaVersion = 0
	b, err := json.Marshal(fields)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// ApplyRuntimeTargetPresentation applies write rules 2-4 to a presentation
// reported for broker brokerID and stores it through the fenced
// SetRuntimeBrokerPresentation. It ignores (and never clears) the stored
// value when p is nil (an older broker), when the flat Runtime Brokers
// experiment is off, or when p's schema version is not one this Hub knows;
// the store refuses a legacy row (ErrRuntimeBrokerNotFlat) and a targetID
// or target type that differs from the stored runtime target
// (ErrRuntimeTargetChanged, carrying both values), which are ignored too.
// The store is the one place that reads the row, so the binding is checked
// on the same read as the fence. epoch and seq are the reporting process's
// fence; now is the Hub time recorded as observed-at and fence time. Rule 1
// (the request is authenticated as the broker itself) is the caller's.
func (s *Server) ApplyRuntimeTargetPresentation(ctx context.Context, brokerID, targetID string, p *api.RuntimeTargetPresentation, epoch, seq int64, now time.Time) (PresentationResult, error) {
	ignore := func(reason string, args ...any) (PresentationResult, error) {
		if reason != presentationIgnoredOmitted {
			s.agentLifecycleLog.Info("runtime target presentation ignored",
				append([]any{"broker_id", brokerID, "reason", reason}, args...)...)
		}
		return PresentationResult{Ignored: reason}, nil
	}
	if p == nil {
		return ignore(presentationIgnoredOmitted)
	}
	if !s.experimentEnabled(experiments.FlatRuntimeBrokers) {
		return ignore(presentationIgnoredExperimentOff)
	}
	if p.SchemaVersion != api.RuntimeTargetPresentationSchemaVersion {
		return ignore(presentationIgnoredSchemaVersion, "schema_version", p.SchemaVersion)
	}
	// The hub stores presented image registries with the same display
	// redaction as the broker record, whatever the reporting broker sent;
	// the hash is computed on the stored (redacted) form.
	p = presentationForStore(p)
	hash, err := RuntimeTargetPresentationHash(p)
	if err != nil {
		return PresentationResult{}, err
	}
	outcome, err := s.store.SetRuntimeBrokerPresentation(ctx, brokerID, targetID, p, hash, epoch, seq, now)
	switch {
	case errors.Is(err, store.ErrRuntimeBrokerNotFlat):
		return ignore(presentationIgnoredLegacyRow)
	case errors.Is(err, store.ErrRuntimeTargetChanged):
		return ignore(presentationIgnoredTarget, "detail", err.Error())
	case err != nil:
		return PresentationResult{}, err
	}
	if outcome == store.PresentationStale {
		s.agentLifecycleLog.Debug("runtime target presentation stale (fence did not advance)",
			"broker_id", brokerID, "epoch", epoch, "seq", seq)
	}
	return PresentationResult{Outcome: outcome}, nil
}

// presentationForStore returns p with its presented image registry redacted
// like the broker record's (config.RedactImageRegistry), copying only what it
// changes; p itself is never modified. The redaction is idempotent, so a
// presentation built by PresentInstance is returned unchanged.
func presentationForStore(p *api.RuntimeTargetPresentation) *api.RuntimeTargetPresentation {
	if p.AgentDefaults == nil || p.AgentDefaults.ImageRegistry == "" {
		return p
	}
	r := config.RedactImageRegistry(p.AgentDefaults.ImageRegistry)
	if r == p.AgentDefaults.ImageRegistry {
		return p
	}
	pc, ad := *p, *p.AgentDefaults
	ad.ImageRegistry = r
	pc.AgentDefaults = &ad
	return &pc
}
