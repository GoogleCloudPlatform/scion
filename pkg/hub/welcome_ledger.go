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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// welcomeLedgerSection is the hub_settings section that records the welcome
// project's one-time lifecycle (ptone/scion#7534). Like
// builtin_resources_seeded it has no opsettings definition, so it is never
// surfaced as an operational setting, and it is written by CAS.
const welcomeLedgerSection = "welcome_project"

// welcomeLedgerSchemaVersion is the schema_version written to the row.
const welcomeLedgerSchemaVersion = 1

// welcomeLedgerUpdatedBy is the updated_by value the welcome flow writes.
const welcomeLedgerUpdatedBy = "welcome"

// Welcome project states.
const (
	welcomeStateClaimed            = "claimed"
	welcomeStatePreviewed          = "previewed"
	welcomeStateCreated            = "created"
	welcomeStatePendingCredentials = "pending_credentials"
	welcomeStatePendingBroker      = "pending_broker"
	welcomeStateStarted            = "started"
	welcomeStateSkipped            = "skipped"
	welcomeStateFailed             = "failed"
)

// welcomeSkillPin records one vendored skill: its canonical repository, the
// ref its URI named ("" for none) and the commit it resolved to.
type welcomeSkillPin struct {
	Repo string `json:"repo"`
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`
}

// welcomeManifest is what the install pinned: the agent-team commit, every
// vendored skill's pin, the installed template names and the aggregate
// sha256 over every staged file (welcomeFilesDigest).
type welcomeManifest struct {
	AgentTeamRepo string            `json:"agent_team_repo"`
	AgentTeamSHA  string            `json:"agent_team_sha"`
	Skills        []welcomeSkillPin `json:"skills"`
	Templates     []string          `json:"templates"`
	FilesSHA256   string            `json:"files_sha256"`
	// SkippedEntries counts link, device and other special archive entries
	// outside the selected subtrees, which were skipped (never extracted).
	SkippedEntries int `json:"skipped_entries"`
}

// welcomeLedgerDoc is the JSON value stored in the hub_settings row.
type welcomeLedgerDoc struct {
	SchemaVersion int    `json:"schema_version"`
	State         string `json:"state"`
	Reason        string `json:"reason,omitempty"`
	OwnerUserID   string `json:"owner_user_id,omitempty"`
	ProjectID     string `json:"project_id,omitempty"`
	// ProjectSlug is the slug the project was actually created with (the
	// next free one when "welcome" is taken).
	ProjectSlug string           `json:"project_slug,omitempty"`
	AgentID     string           `json:"agent_id,omitempty"`
	Manifest    *welcomeManifest `json:"manifest,omitempty"`
	UpdatedAt   time.Time        `json:"updated_at"`
}

// welcomeLedger is a loaded ledger row: its document and the revision the
// next CAS write must name (0 when no row exists).
type welcomeLedger struct {
	doc welcomeLedgerDoc
	rev int64
}

// exists reports whether a row was loaded.
func (l *welcomeLedger) exists() bool { return l != nil && l.rev > 0 }

// welcomeStateFinal reports whether state ends the flow for good: once the
// project exists, or the flow was skipped or failed, nothing is created
// again, whatever happens to the project afterwards.
func welcomeStateFinal(state string) bool {
	switch state {
	case welcomeStateCreated, welcomeStatePendingCredentials, welcomeStatePendingBroker,
		welcomeStateStarted, welcomeStateSkipped, welcomeStateFailed:
		return true
	}
	return false
}

// errWelcomeLedgerConflict reports that another writer changed the ledger
// between our read and our CAS write.
var errWelcomeLedgerConflict = errors.New("the welcome project ledger changed concurrently")

// loadWelcomeLedger reads the ledger row. A missing row yields an empty
// ledger with rev 0.
func (s *Server) loadWelcomeLedger(ctx context.Context) (*welcomeLedger, error) {
	row, err := s.store.GetHubSetting(ctx, welcomeLedgerSection)
	if errors.Is(err, store.ErrNotFound) {
		return &welcomeLedger{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", welcomeLedgerSection, err)
	}
	l := &welcomeLedger{rev: row.Revision}
	if len(row.Value) > 0 {
		if err := json.Unmarshal(row.Value, &l.doc); err != nil {
			return nil, fmt.Errorf("decode %s: %w", welcomeLedgerSection, err)
		}
	}
	return l, nil
}

// saveWelcomeLedger writes doc over l by CAS: create-only when l has no row,
// otherwise only if the row is still at l's revision. A lost race returns
// errWelcomeLedgerConflict and writes nothing; the caller reloads and
// re-decides (a state change is never blindly re-applied). On success l
// holds the written document and its new revision.
func (s *Server) saveWelcomeLedger(ctx context.Context, l *welcomeLedger, doc welcomeLedgerDoc) error {
	doc.SchemaVersion = welcomeLedgerSchemaVersion
	doc.UpdatedAt = s.welcomeNow().UTC()
	value, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("encode %s: %w", welcomeLedgerSection, err)
	}
	row, err := s.store.UpsertHubSetting(ctx, welcomeLedgerSection, value, welcomeLedgerUpdatedBy, l.rev, "seeded")
	if errors.Is(err, store.ErrRevisionConflict) {
		return errWelcomeLedgerConflict
	}
	if err != nil {
		return fmt.Errorf("save %s: %w", welcomeLedgerSection, err)
	}
	l.doc = doc
	l.rev = row.Revision
	return nil
}

// transitionWelcomeLedger moves the ledger from one of the states in from
// ("" meaning no row) to doc's state. It reloads first, so a caller holding
// a stale copy cannot overwrite a newer state; a ledger in any other state
// returns errWelcomeLedgerConflict.
func (s *Server) transitionWelcomeLedger(ctx context.Context, doc welcomeLedgerDoc, from ...string) (*welcomeLedger, error) {
	l, err := s.loadWelcomeLedger(ctx)
	if err != nil {
		return nil, err
	}
	current := ""
	if l.exists() {
		current = l.doc.State
	}
	allowed := false
	for _, f := range from {
		if f == current {
			allowed = true
			break
		}
	}
	if !allowed {
		return l, errWelcomeLedgerConflict
	}
	if err := s.saveWelcomeLedger(ctx, l, doc); err != nil {
		return l, err
	}
	return l, nil
}
