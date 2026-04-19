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

package entadapter

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/workflowrun"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// WorkflowRunStore implements store.WorkflowRunStore using Ent ORM.
type WorkflowRunStore struct {
	client *ent.Client
}

// NewWorkflowRunStore creates a new Ent-backed WorkflowRunStore.
func NewWorkflowRunStore(client *ent.Client) *WorkflowRunStore {
	return &WorkflowRunStore{client: client}
}

// entWorkflowRunToStore converts an Ent WorkflowRun entity to a store.WorkflowRun model.
func entWorkflowRunToStore(r *ent.WorkflowRun) *store.WorkflowRun {
	m := &store.WorkflowRun{
		ID:         r.ID.String(),
		GroveID:    r.GroveID.String(),
		SourceYaml: r.SourceYaml,
		Status:     string(r.Status),
		Created:    r.Created,
		Updated:    r.Updated,
	}
	if r.BrokerID != nil {
		m.BrokerID = r.BrokerID
	}
	if r.InputsJSON != nil {
		s := string(*r.InputsJSON)
		m.InputsJSON = s
	}
	if r.ResultJSON != nil {
		s := string(*r.ResultJSON)
		m.ResultJSON = &s
	}
	if r.ErrorMessage != nil {
		m.ErrorMessage = r.ErrorMessage
	}
	if r.TraceURL != nil {
		m.TraceURL = r.TraceURL
	}
	if r.StartedAt != nil {
		m.StartedAt = r.StartedAt
	}
	if r.FinishedAt != nil {
		m.FinishedAt = r.FinishedAt
	}
	if r.CreatedByUserID != nil {
		s := r.CreatedByUserID.String()
		m.CreatedByUserID = &s
	}
	if r.CreatedByAgentID != nil {
		s := r.CreatedByAgentID.String()
		m.CreatedByAgentID = &s
	}
	return m
}

// CreateWorkflowRun persists a new workflow run.
func (s *WorkflowRunStore) CreateWorkflowRun(ctx context.Context, run *store.WorkflowRun) error {
	groveID, err := parseUUID(run.GroveID)
	if err != nil {
		return fmt.Errorf("%w: invalid grove ID: %v", store.ErrInvalidInput, err)
	}

	id, err := parseUUID(run.ID)
	if err != nil {
		return fmt.Errorf("%w: invalid run ID: %v", store.ErrInvalidInput, err)
	}

	c := s.client.WorkflowRun.Create().
		SetID(id).
		SetGroveID(groveID).
		SetSourceYaml(run.SourceYaml)

	if run.InputsJSON != "" {
		c = c.SetInputsJSON([]byte(run.InputsJSON))
	}

	if run.CreatedByUserID != nil {
		uid, err := parseUUID(*run.CreatedByUserID)
		if err == nil {
			c = c.SetCreatedByUserID(uid)
		}
	}
	if run.CreatedByAgentID != nil {
		aid, err := parseUUID(*run.CreatedByAgentID)
		if err == nil {
			c = c.SetCreatedByAgentID(aid)
		}
	}

	if _, err := c.Save(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// GetWorkflowRun retrieves a workflow run by ID.
func (s *WorkflowRunStore) GetWorkflowRun(ctx context.Context, id string) (*store.WorkflowRun, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return nil, store.ErrNotFound
	}
	r, err := s.client.WorkflowRun.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entWorkflowRunToStore(r), nil
}

// ListWorkflowRuns returns workflow runs matching the filter, with cursor-based pagination.
func (s *WorkflowRunStore) ListWorkflowRuns(ctx context.Context, opts store.WorkflowRunListOptions) (*store.ListResult[store.WorkflowRun], error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	q := s.client.WorkflowRun.Query().
		Order(ent.Desc(workflowrun.FieldCreated)).
		Limit(limit + 1) // fetch one extra to detect next page

	if opts.Filter.GroveID != "" {
		groveID, err := parseUUID(opts.Filter.GroveID)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid grove ID: %v", store.ErrInvalidInput, err)
		}
		q = q.Where(workflowrun.GroveID(groveID))
	}

	if opts.Filter.Status != "" {
		q = q.Where(workflowrun.StatusEQ(workflowrun.Status(opts.Filter.Status)))
	}

	// Cursor-based pagination: cursor encodes the offset as base64(strconv.Itoa(offset))
	offset := 0
	if opts.Cursor != "" {
		decoded, err := base64.StdEncoding.DecodeString(opts.Cursor)
		if err == nil {
			if n, err := strconv.Atoi(string(decoded)); err == nil && n >= 0 {
				offset = n
			}
		}
	}
	if offset > 0 {
		q = q.Offset(offset)
	}

	rows, err := q.All(ctx)
	if err != nil {
		return nil, mapError(err)
	}

	var nextCursor string
	if len(rows) > limit {
		rows = rows[:limit]
		nextOffset := offset + limit
		nextCursor = base64.StdEncoding.EncodeToString([]byte(strconv.Itoa(nextOffset)))
	}

	items := make([]store.WorkflowRun, len(rows))
	for i, r := range rows {
		items[i] = *entWorkflowRunToStore(r)
	}

	return &store.ListResult[store.WorkflowRun]{
		Items:      items,
		NextCursor: nextCursor,
	}, nil
}

// CancelWorkflowRun atomically sets status to "canceled" for non-terminal runs.
//
// Per design doc Section 3.5, cancellation is idempotent: if the run is already
// in a terminal state, the current run is returned without error. The handler
// distinguishes between a fresh cancellation (202 Accepted) and a no-op on an
// already-terminal run (200 OK) via the returned alreadyTerminal flag.
func (s *WorkflowRunStore) CancelWorkflowRun(ctx context.Context, id string) (*store.WorkflowRun, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return nil, store.ErrNotFound
	}

	// Load the current run.
	r, err := s.client.WorkflowRun.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}

	// If already terminal, return the current run without modification.
	// ErrVersionConflict signals to the handler that no state change occurred.
	switch r.Status {
	case workflowrun.StatusSucceeded, workflowrun.StatusFailed, workflowrun.StatusCanceled, workflowrun.StatusTimedOut:
		return entWorkflowRunToStore(r), store.ErrVersionConflict
	}

	// Attempt atomic CAS update: only update if status is still a non-terminal value.
	updated, err := s.client.WorkflowRun.UpdateOneID(uid).
		SetStatus(workflowrun.StatusCanceled).
		Where(
			workflowrun.StatusIn(
				workflowrun.StatusQueued,
				workflowrun.StatusProvisioning,
				workflowrun.StatusRunning,
			),
		).
		Save(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			// Status was changed between the Get and UpdateOneID — re-fetch to
			// return the current terminal state.
			current, getErr := s.client.WorkflowRun.Get(ctx, uid)
			if getErr != nil {
				return nil, mapError(getErr)
			}
			return entWorkflowRunToStore(current), store.ErrVersionConflict
		}
		return nil, mapError(err)
	}
	return entWorkflowRunToStore(updated), nil
}

// TransitionWorkflowRun applies a lifecycle status update, optionally guarded by fromStatus.
func (s *WorkflowRunStore) TransitionWorkflowRun(ctx context.Context, id string, update store.WorkflowRunTransition, fromStatus []string) (*store.WorkflowRun, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return nil, store.ErrNotFound
	}

	// CAS guard: check current status before updating.
	if len(fromStatus) > 0 {
		r, err := s.client.WorkflowRun.Get(ctx, uid)
		if err != nil {
			return nil, mapError(err)
		}
		currentStatus := string(r.Status)
		allowed := false
		for _, fs := range fromStatus {
			if currentStatus == fs {
				allowed = true
				break
			}
		}
		if !allowed {
			return entWorkflowRunToStore(r), store.ErrVersionConflict
		}
	}

	u := s.client.WorkflowRun.UpdateOneID(uid).
		SetStatus(workflowrun.Status(update.Status))

	if update.BrokerID != nil {
		u = u.SetBrokerID(*update.BrokerID)
	}
	if update.StartedAt != nil {
		u = u.SetStartedAt(*update.StartedAt)
	}
	if update.FinishedAt != nil {
		u = u.SetFinishedAt(*update.FinishedAt)
	}
	if update.ResultJSON != nil {
		u = u.SetResultJSON([]byte(*update.ResultJSON))
	}
	if update.ErrorMessage != nil {
		u = u.SetErrorMessage(*update.ErrorMessage)
	}
	if update.TraceURL != nil {
		u = u.SetTraceURL(*update.TraceURL)
	}

	updated, err := u.Save(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, store.ErrNotFound
		}
		return nil, mapError(err)
	}
	return entWorkflowRunToStore(updated), nil
}

// Ensure WorkflowRunStore implements store.WorkflowRunStore.
var _ store.WorkflowRunStore = (*WorkflowRunStore)(nil)

// parseUUIDPtr parses an optional UUID string.
func parseUUIDPtr(s *string) *uuid.UUID {
	if s == nil {
		return nil
	}
	uid, err := uuid.Parse(*s)
	if err != nil {
		return nil
	}
	return &uid
}
