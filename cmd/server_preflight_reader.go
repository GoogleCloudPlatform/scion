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

package cmd

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	entagent "github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/hubsetting"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/project"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/projectcontributor"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/preflight"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// preflightReader implements preflight.ReadOnlyQueries with
// column-restricted queries (an explicit Select on each), all inside one
// read-only snapshot transaction. It only queries: it creates, updates and
// deletes nothing. A release that adds columns to these tables does not
// break it; only a rename or drop of a column it reads does.
type preflightReader struct {
	q           *ent.Client // the snapshot transaction's client
	colocatedID string
}

// newPreflightReader returns the reader that queries through q, the
// client of the one snapshot transaction (ent.Tx.Client).
func newPreflightReader(q *ent.Client, colocatedID string) *preflightReader {
	return &preflightReader{q: q, colocatedID: colocatedID}
}

var _ preflight.ReadOnlyQueries = (*preflightReader)(nil)

// Broker reports whether broker id has a row and whether it is a flat
// instance (a non-empty runtime_target_id). An ID that is not a UUID has
// no row.
func (r *preflightReader) Broker(ctx context.Context, id string) (found, flat bool, err error) {
	uid, err := uuid.Parse(id)
	if err != nil {
		return false, false, nil
	}
	b, err := r.q.RuntimeBroker.Query().
		Where(runtimebroker.IDEQ(uid)).
		Select(runtimebroker.FieldRuntimeTargetID).
		Only(ctx)
	if ent.IsNotFound(err) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if b.RuntimeTargetID != nil && *b.RuntimeTargetID != "" {
		flat = true
	}
	return true, flat, nil
}

// hubSettingScanRow is a hub_settings row as read. updated_by is scanned
// into a pointer because the entity maps NULL to "", and the boot-time
// sync treats a NULL updated_by differently from any value.
type hubSettingScanRow struct {
	Section   string  `sql:"section"`
	Value     []byte  `sql:"value"`
	Origin    string  `sql:"origin"`
	UpdatedBy *string `sql:"updated_by"`
}

// HubSettings returns the rows of the given sections that exist.
func (r *preflightReader) HubSettings(ctx context.Context, sections []string) ([]preflight.HubSettingRow, error) {
	var scanned []hubSettingScanRow
	err := r.q.HubSetting.Query().
		Where(hubsetting.SectionIn(sections...)).
		Select(hubsetting.FieldSection, hubsetting.FieldValue, hubsetting.FieldOrigin, hubsetting.FieldUpdatedBy).
		Scan(ctx, &scanned)
	if err != nil {
		return nil, err
	}
	rows := make([]preflight.HubSettingRow, 0, len(scanned))
	for _, s := range scanned {
		row := preflight.HubSettingRow{Section: s.Section, Origin: s.Origin, Value: json.RawMessage(s.Value)}
		if s.UpdatedBy != nil {
			row.UpdatedBy = s.UpdatedBy
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// Agents returns the non-deleted agents. applied_config decodes as the
// hub's agent store decodes it (store.AppliedConfigProfile); a row that
// does not decode gives AppliedProfile "" and, when the count includes it
// (agent.CountsOnBroker), is tallied in unparseable.
func (r *preflightReader) Agents(ctx context.Context) ([]agent.ProfileRefAgentRow, int, error) {
	as, err := r.q.Agent.Query().
		Where(entagent.DeletedAtIsNil()).
		Select(entagent.FieldSlug, entagent.FieldProjectID, entagent.FieldRuntimeBrokerID, entagent.FieldAppliedConfig).
		All(ctx)
	if err != nil {
		return nil, 0, err
	}
	rows := make([]agent.ProfileRefAgentRow, 0, len(as))
	unparseable := 0
	for _, a := range as {
		profile, decoded := store.AppliedConfigProfile(a.AppliedConfig)
		if !decoded {
			if agent.CountsOnBroker(a.RuntimeBrokerID, r.colocatedID) {
				unparseable++
			}
		}
		rows = append(rows, agent.ProfileRefAgentRow{
			Slug:            a.Slug,
			ProjectID:       a.ProjectID.String(),
			RuntimeBrokerID: a.RuntimeBrokerID,
			AppliedProfile:  profile,
		})
	}
	return rows, unparseable, nil
}

// Projects returns every project. SharedWorkspace is evaluated through
// store.Project.IsSharedWorkspace on the two columns it reads.
func (r *preflightReader) Projects(ctx context.Context) ([]agent.ProfileRefProject, error) {
	ps, err := r.q.Project.Query().
		Select(project.FieldSlug, project.FieldGitRemote, project.FieldLabels, project.FieldAnnotations).
		All(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]agent.ProfileRefProject, 0, len(ps))
	for _, p := range ps {
		sp := store.Project{
			Labels: p.Labels,
		}
		if p.GitRemote != nil {
			sp.GitRemote = *p.GitRemote
		}
		rows = append(rows, agent.ProfileRefProject{
			ID:              p.ID.String(),
			Slug:            p.Slug,
			ActiveProfile:   p.Annotations[store.AnnotationProjectActiveProfile],
			SharedWorkspace: sp.IsSharedWorkspace(),
		})
	}
	return rows, nil
}

// Providers returns the provider rows of broker brokerID. An ID that is
// not a UUID has none.
func (r *preflightReader) Providers(ctx context.Context, brokerID string) ([]agent.ProfileRefProvider, error) {
	uid, err := uuid.Parse(brokerID)
	if err != nil {
		return nil, nil
	}
	cs, err := r.q.ProjectContributor.Query().
		Where(projectcontributor.BrokerIDEQ(uid)).
		Select(projectcontributor.FieldProjectID, projectcontributor.FieldLocalPath).
		All(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]agent.ProfileRefProvider, 0, len(cs))
	for _, c := range cs {
		rows = append(rows, agent.ProfileRefProvider{ProjectID: c.ProjectID.String(), LocalPath: c.LocalPath})
	}
	return rows, nil
}
