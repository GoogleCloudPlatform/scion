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

// Package testinfra reads and writes the test-infrastructure marker: the
// single row of the test_infra_marker table that designates a database as
// the test database of one hub (ptone/scion#4240, admin tier phase A0, gate
// condition G6).
//
// The package is deliberately outside pkg/hub and pkg/store:
//   - Mark is called only by the offline host command
//     `scion server test-infra mark` (cmd/server_test_infra.go). No hub
//     route and no store.Store method can reach it; a pkg/hub test asserts
//     that no non-test pkg/hub file references Mark or the marker's ent
//     create builder.
//   - Read is called only at server startup, by the test admin gate in cmd,
//     before the hub serves any request.
//
// The one generic code path that walks the table is the hub-maintenance
// timestamp normalization (pkg/store/entadapter/timestamp_normalize.go),
// which may rewrite the text format of created_at (same instant). It never
// touches hub_id or class.
package testinfra

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

// MarkerID is the fixed id of the single marker row.
const MarkerID = "test-infra-marker"

// MarkMutationType is the mutation_audits.mutation_type of the audit row the
// mark command writes in the same transaction as the marker.
const MarkMutationType = "test_infra_mark"

// ErrMarkerAbsent is returned by Read when the database has no marker row.
var ErrMarkerAbsent = errors.New("test infrastructure marker absent")

// ErrMarkerExists is returned by Mark when the database already has a
// marker row. Re-designating a database is a deliberate host-side act:
// remove the row first.
var ErrMarkerExists = errors.New("test infrastructure marker already present")

// Marker is the stored marker row.
type Marker struct {
	HubID         string
	Class         string
	CreatedAt     time.Time
	CreatedByHost string
	// Attested holds operator-declared substrate facts (for example the
	// storage bucket, secret-backend project and broker ids). They are
	// recorded, not verified.
	Attested json.RawMessage
}

// Read returns the marker row, or ErrMarkerAbsent when there is none.
func Read(ctx context.Context, client *ent.Client) (*Marker, error) {
	if client == nil {
		return nil, errors.New("test infrastructure marker: no database client")
	}
	row, err := client.TestInfraMarker.Get(ctx, MarkerID)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrMarkerAbsent
		}
		return nil, fmt.Errorf("read test infrastructure marker: %w", err)
	}
	return &Marker{
		HubID:         row.HubID,
		Class:         row.Class,
		CreatedAt:     row.CreatedAt,
		CreatedByHost: row.CreatedByHost,
		Attested:      row.Attested,
	}, nil
}

// MarkInput is the input of Mark.
type MarkInput struct {
	// HubID is the resolved hub id the database is designated for.
	HubID string
	// Operator identifies who ran the command, for the audit row.
	Operator string
	// Host is the host name the command ran on.
	Host string
	// Attested is the operator-declared substrate JSON (an object), or nil.
	Attested json.RawMessage
	// Now is the marking time (wall clock of the host).
	Now time.Time
}

// Mark writes the marker row (class "test") and a test_infra_mark mutation
// audit row in one transaction. It refuses when a marker already exists.
func Mark(ctx context.Context, client *ent.Client, in MarkInput) (*Marker, error) {
	if client == nil {
		return nil, errors.New("test infrastructure marker: no database client")
	}
	hubID := strings.TrimSpace(in.HubID)
	if hubID == "" || hubID != in.HubID {
		return nil, errors.New("test infrastructure marker: hub id must be non-empty and carry no surrounding whitespace")
	}
	if len(in.Attested) > 0 {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(in.Attested, &obj); err != nil || obj == nil {
			return nil, errors.New("test infrastructure marker: attested must be a JSON object")
		}
	}
	operator := strings.TrimSpace(in.Operator)
	if operator == "" {
		operator = "unknown"
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()

	tx, err := client.Tx(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	rollback := func(cause error) (*Marker, error) {
		_ = tx.Rollback()
		return nil, cause
	}

	exists, err := tx.TestInfraMarker.Query().Exist(ctx)
	if err != nil {
		return rollback(fmt.Errorf("read test infrastructure marker: %w", err))
	}
	if exists {
		return rollback(ErrMarkerExists)
	}

	create := tx.TestInfraMarker.Create().
		SetID(MarkerID).
		SetHubID(hubID).
		SetClass(config.HubClassTest).
		SetCreatedAt(now).
		SetCreatedByHost(in.Host)
	if len(in.Attested) > 0 {
		create.SetAttested(in.Attested)
	}
	row, err := create.Save(ctx)
	if err != nil {
		if ent.IsConstraintError(err) {
			return rollback(ErrMarkerExists)
		}
		return rollback(fmt.Errorf("write test infrastructure marker: %w", err))
	}

	audit := entadapter.NewMutationAuditStore(tx.Client())
	if err := audit.CreateMutationAudit(ctx, &store.MutationAuditRecord{
		Timestamp:          now,
		MutationType:       MarkMutationType,
		ActorPrincipalKind: "operator",
		ActorPrincipalID:   operator,
		TargetType:         "test_infra_marker",
		TargetID:           MarkerID,
		AfterSummary: fmt.Sprintf("test infrastructure marker written: hub_id=%s class=%s host=%s operator=%s",
			hubID, config.HubClassTest, in.Host, operator),
	}); err != nil {
		return rollback(fmt.Errorf("write test_infra_mark audit row: %w", err))
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit test infrastructure marker: %w", err)
	}
	return &Marker{
		HubID:         row.HubID,
		Class:         row.Class,
		CreatedAt:     row.CreatedAt,
		CreatedByHost: row.CreatedByHost,
		Attested:      row.Attested,
	}, nil
}
