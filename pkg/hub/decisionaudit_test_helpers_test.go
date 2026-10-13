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
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/auditevent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// capturingAuditEmitter records every DecisionAuditRecord it receives, for
// tests that need to inspect audit content rather than merely count calls.
//
// This type carries no sqlite dependency of its own, so it lives in a file
// without a no_sqlite build constraint: a caller under either build
// configuration must be able to resolve it. It moved here out of
// authz_broker_obo_test.go (which does carry the !no_sqlite constraint for
// its other, store-backed tests) so that identity_classification_test.go,
// which has no such constraint, keeps resolving it under -tags no_sqlite.
type capturingAuditEmitter struct {
	mu      sync.Mutex
	records []*store.DecisionAuditRecord
}

func (e *capturingAuditEmitter) EmitDecisionAudit(_ context.Context, record *store.DecisionAuditRecord) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.records = append(e.records, record)
}

func (e *capturingAuditEmitter) last() *store.DecisionAuditRecord {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.records) == 0 {
		return nil
	}
	return e.records[len(e.records)-1]
}

// decisionLogCapture is a process-default slog.Handler for decision-log
// tests (remaining-audit P1). Installed with slog.SetDefault before New, it
// is what the audit writer captures as its inner handler. It keeps only
// scion.audit records, rendered as JSON objects; every other record (for
// example from subsystem loggers built in New) is accepted and dropped.
//
// Optional faults for audit records only: entered is signalled (without
// blocking) when Handle starts, block makes Handle wait until it is
// closed, and fail is returned instead of capturing.
type decisionLogCapture struct {
	mu      sync.Mutex
	lines   []map[string]any
	entered chan struct{}
	block   chan struct{}
	fail    error
}

func (c *decisionLogCapture) Enabled(context.Context, slog.Level) bool { return true }

func (c *decisionLogCapture) Handle(_ context.Context, r slog.Record) error {
	if r.Message != auditevent.EventName {
		return nil
	}
	if c.entered != nil {
		select {
		case c.entered <- struct{}{}:
		default:
		}
	}
	if c.block != nil {
		<-c.block
	}
	if c.fail != nil {
		return c.fail
	}
	var buf bytes.Buffer
	if err := slog.NewJSONHandler(&buf, nil).Handle(context.Background(), r); err != nil {
		return err
	}
	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		return err
	}
	c.mu.Lock()
	c.lines = append(c.lines, line)
	c.mu.Unlock()
	return nil
}

func (c *decisionLogCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *decisionLogCapture) WithGroup(string) slog.Handler      { return c }

// records returns the captured scion.audit records.
func (c *decisionLogCapture) records() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]map[string]any(nil), c.lines...)
}

// recordsFor returns the captured records whose correlation_id is id.
func (c *decisionLogCapture) recordsFor(id string) []map[string]any {
	var out []map[string]any
	for _, line := range c.records() {
		if line["correlation_id"] == id {
			out = append(out, line)
		}
	}
	return out
}
