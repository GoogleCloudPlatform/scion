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
//   - Read is called only at server startup, by the --test-mode gate in cmd,
//     before the hub serves any request. ReadWebSessionKey is likewise
//     called only at server startup, in cmd, when test identities are on.
//   - Unmark, like Mark, is called only by the offline host command
//     (`scion server test-infra unmark`).
//
// The marker row also holds the hub-local test-identity web session key
// (phase WS1, DEC-8): 64 bytes from crypto/rand that Mark generates. It is
// never derived from any other input, never logged, and never part of
// Marker. Unmark deletes it with the row, and a later Mark generates a new
// one, so every test-identity web session cookie signed with the old key
// stops validating.
//
// The one generic code path that walks the table is the hub-maintenance
// timestamp normalization (pkg/store/entadapter/timestamp_normalize.go),
// which may rewrite the text format of created_at (same instant). It never
// touches hub_id or class.
package testinfra

import (
	"bytes"
	"context"
	"crypto/rand"
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

// UnmarkMutationType is the mutation_audits.mutation_type of the audit row
// the unmark command writes in the same transaction as the delete.
const UnmarkMutationType = "test_infra_unmark"

// WebSessionKeyLen is the length in bytes of the test-identity web session
// key (512 bits; the hub expands it into a 32-byte hash key and a 32-byte
// block key).
const WebSessionKeyLen = 64

// Errors of ReadWebSessionKey besides ErrMarkerAbsent. Each one means the
// hub runs without test-identity web sessions; none of them is fatal.
var (
	ErrWebSessionKeyMissing     = errors.New("test infrastructure marker has no web session key; mark the database again")
	ErrWebSessionKeyInvalid     = errors.New("test infrastructure marker web session key is malformed")
	ErrWebSessionKeyHubMismatch = errors.New("test infrastructure marker names a different hub id")
	ErrWebSessionKeyClass       = errors.New(`test infrastructure marker class is not "test"`)
)

// randRead is the key source. Tests replace it to simulate a failure.
var randRead = rand.Read

// newWebSessionKey returns WebSessionKeyLen bytes from crypto/rand.
func newWebSessionKey() ([]byte, error) {
	key := make([]byte, WebSessionKeyLen)
	if _, err := randRead(key); err != nil {
		return nil, fmt.Errorf("generate web session key: %w", err)
	}
	if bytes.Equal(key, make([]byte, WebSessionKeyLen)) {
		return nil, errors.New("generate web session key: random source returned zeros")
	}
	return key, nil
}

// ReadWebSessionKey returns the hub-local test-identity web session key
// from the marker row. It requires the marker to name hubID and class
// "test", and the key to be exactly WebSessionKeyLen bytes, not all zero.
// It returns ErrMarkerAbsent, ErrWebSessionKeyHubMismatch,
// ErrWebSessionKeyClass, ErrWebSessionKeyMissing, ErrWebSessionKeyInvalid
// or a wrapped read error otherwise. The caller must never log the key.
func ReadWebSessionKey(ctx context.Context, client *ent.Client, hubID string) ([]byte, error) {
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
	switch {
	case hubID == "" || row.HubID != hubID:
		return nil, ErrWebSessionKeyHubMismatch
	case !config.IsTestHubClass(row.Class):
		return nil, ErrWebSessionKeyClass
	case len(row.WebSessionKey) == 0:
		return nil, ErrWebSessionKeyMissing
	case len(row.WebSessionKey) != WebSessionKeyLen || bytes.Equal(row.WebSessionKey, make([]byte, WebSessionKeyLen)):
		return nil, ErrWebSessionKeyInvalid
	}
	key := make([]byte, WebSessionKeyLen)
	copy(key, row.WebSessionKey)
	return key, nil
}

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

	webSessionKey, err := newWebSessionKey()
	if err != nil {
		return nil, err
	}

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
		SetCreatedByHost(in.Host).
		SetWebSessionKey(webSessionKey)
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

// UnmarkInput is the input of Unmark.
type UnmarkInput struct {
	// Operator identifies who ran the command, for the audit row.
	Operator string
	// Host is the host name the command ran on.
	Host string
	// Now is the unmarking time (wall clock of the host).
	Now time.Time
}

// Unmark deletes the marker row, and with it the test-identity web session
// key, and writes a test_infra_unmark mutation audit row in one
// transaction. It returns the deleted marker, or ErrMarkerAbsent when there
// is none.
func Unmark(ctx context.Context, client *ent.Client, in UnmarkInput) (*Marker, error) {
	if client == nil {
		return nil, errors.New("test infrastructure marker: no database client")
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
	row, err := tx.TestInfraMarker.Get(ctx, MarkerID)
	if err != nil {
		if ent.IsNotFound(err) {
			return rollback(ErrMarkerAbsent)
		}
		return rollback(fmt.Errorf("read test infrastructure marker: %w", err))
	}
	if err := tx.TestInfraMarker.DeleteOneID(MarkerID).Exec(ctx); err != nil {
		return rollback(fmt.Errorf("delete test infrastructure marker: %w", err))
	}
	audit := entadapter.NewMutationAuditStore(tx.Client())
	if err := audit.CreateMutationAudit(ctx, &store.MutationAuditRecord{
		Timestamp:          now,
		MutationType:       UnmarkMutationType,
		ActorPrincipalKind: "operator",
		ActorPrincipalID:   operator,
		TargetType:         "test_infra_marker",
		TargetID:           MarkerID,
		BeforeSummary: fmt.Sprintf("test infrastructure marker deleted: hub_id=%s class=%s host=%s operator=%s",
			row.HubID, row.Class, in.Host, operator),
	}); err != nil {
		return rollback(fmt.Errorf("write test_infra_unmark audit row: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit test infrastructure marker delete: %w", err)
	}
	return &Marker{
		HubID:         row.HubID,
		Class:         row.Class,
		CreatedAt:     row.CreatedAt,
		CreatedByHost: row.CreatedByHost,
		Attested:      row.Attested,
	}, nil
}
