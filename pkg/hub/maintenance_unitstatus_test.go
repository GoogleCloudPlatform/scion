//go:build !no_sqlite

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
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/unitdrift"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func newUnitStatusStore(t *testing.T) store.Store {
	t.Helper()
	st, err := newTestStore(t, ":memory:")
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	return st
}

func readUnitStatus(t *testing.T, st store.Store) (UnitDriftStatus, string, bool) {
	t.Helper()
	setting, err := st.GetHubSetting(context.Background(), HubSettingSectionUnitDrift)
	if errors.Is(err, store.ErrNotFound) {
		return UnitDriftStatus{}, "", false
	}
	if err != nil {
		t.Fatalf("read unit status: %v", err)
	}
	var v UnitDriftStatus
	if err := json.Unmarshal(setting.Value, &v); err != nil {
		t.Fatalf("decode unit status: %v", err)
	}
	return v, string(setting.Value), true
}

func seedDriftRecord(t *testing.T, st store.Store) string {
	t.Helper()
	value := `{"status":"drift","checkedAt":"2026-01-01T00:00:00Z","commit":"old1234","branch":"main","service":"scion-hub.service","summary":["installed: X=1"]}`
	if _, err := st.UpsertHubSetting(context.Background(), HubSettingSectionUnitDrift, json.RawMessage(value), "system", -1, "managed"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, raw, _ := readUnitStatus(t, st)
	return raw
}

func TestRebuildServerExecutor_UnitStatusRecordedBeforeRestart(t *testing.T) {
	f := newRebuildUnitFixture(t, renderedRebuildUnit("ExecStartPre=/bin/true\nEnvironment=\"API_TOKEN=s3cr3t-value\"\n"))
	st := newUnitStatusStore(t)
	var buf syncBuffer
	order := &orderingStore{Store: st, log: &buf, calls: f.calls}
	o := f.options(nil)
	o.Store = order
	e := NewRebuildServerExecutor(o)
	if err := e.Run(context.Background(), &buf, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !f.restartIssued(t) {
		t.Fatal("restart not issued")
	}
	if order.upserts != 1 || order.restartBegunAtUpsert {
		t.Fatalf("the record must be stored once, before the restart begins (upserts %d, restart begun at upsert %v):\n%s", order.upserts, order.restartBegunAtUpsert, buf.String())
	}
	v, raw, ok := readUnitStatus(t, st)
	if !ok {
		t.Fatal("no unit status recorded")
	}
	if v.Status != "drift" || v.Commit != "abc1234" || v.Branch != "main" || v.Service != "scion-hub.service" || v.CheckedAt == "" {
		t.Fatalf("recorded status = %+v", v)
	}
	if !strings.Contains(strings.Join(v.Summary, "\n"), "installed: ExecStartPre=/bin/true") {
		t.Errorf("summary missing the added line: %q", v.Summary)
	}
	if strings.Contains(raw, "s3cr3t-value") || !strings.Contains(strings.Join(v.Summary, "\n"), `installed: Environment="API_TOKEN=…"`) {
		t.Errorf("stored summary must carry only elided values: %s", raw)
	}
}

func TestRebuildServerExecutor_UnitStatusMatchReplacesDrift(t *testing.T) {
	f := newRebuildUnitFixture(t, renderedRebuildUnit(""))
	st := newUnitStatusStore(t)
	seedDriftRecord(t, st)
	o := f.options(nil)
	o.Store = st
	e := NewRebuildServerExecutor(o)
	var buf syncBuffer
	if err := e.Run(context.Background(), &buf, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !f.restartIssued(t) {
		t.Fatal("restart not issued")
	}
	if v, _, ok := readUnitStatus(t, st); !ok || v.Status != "match" || len(v.Summary) != 0 {
		t.Fatalf("recorded status = %+v (present %v), want match", v, ok)
	}
}

func TestRebuildServerExecutor_UnitStatusTransientSkipKeepsDrift(t *testing.T) {
	f := newRebuildUnitFixture(t, renderedRebuildUnit(""))
	st := newUnitStatusStore(t)
	before := seedDriftRecord(t, st)
	o := f.options(func(context.Context, string) (unitdrift.UnitInfo, error) {
		return unitdrift.UnitInfo{}, errors.New("systemd not answering")
	})
	o.Store = st
	e := NewRebuildServerExecutor(o)
	var buf syncBuffer
	if err := e.Run(context.Background(), &buf, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !f.restartIssued(t) {
		t.Fatal("restart not issued")
	}
	if _, after, ok := readUnitStatus(t, st); !ok || after != before {
		t.Fatalf("a transient skip must keep the drift record: before %s, after %s (present %v)", before, after, ok)
	}
}

func TestRebuildServerExecutor_UnitStatusStructuralSkipClearsDrift(t *testing.T) {
	f := newRebuildUnitFixture(t, "[Unit]\nDescription=Not a starter hub\n")
	st := newUnitStatusStore(t)
	seedDriftRecord(t, st)
	o := f.options(nil)
	o.Store = st
	e := NewRebuildServerExecutor(o)
	var buf syncBuffer
	if err := e.Run(context.Background(), &buf, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !f.restartIssued(t) {
		t.Fatal("restart not issued")
	}
	if v, _, ok := readUnitStatus(t, st); ok {
		t.Fatalf("a structural skip must remove the record, found %+v", v)
	}
}

// orderingStore records, when the unit status is stored, whether the
// restart had already begun: its log header written or its command run.
type orderingStore struct {
	store.Store
	log                  *syncBuffer
	calls                string
	upserts              int
	restartBegunAtUpsert bool
}

func (o *orderingStore) UpsertHubSetting(ctx context.Context, section string, value json.RawMessage, by string, rev int64, origin string) (*store.HubSetting, error) {
	if section == HubSettingSectionUnitDrift {
		o.upserts++
		calls, _ := os.ReadFile(o.calls)
		if strings.Contains(o.log.String(), "==> Restarting service") || strings.Contains(string(calls), "systemctl restart") {
			o.restartBegunAtUpsert = true
		}
	}
	return o.Store.UpsertHubSetting(ctx, section, value, by, rev, origin)
}

// failingUnitStatusStore fails every hub setting write.
type failingUnitStatusStore struct{ store.Store }

func (failingUnitStatusStore) UpsertHubSetting(context.Context, string, json.RawMessage, string, int64, string) (*store.HubSetting, error) {
	return nil, errors.New("database unavailable")
}

func TestRebuildServerExecutor_UnitStatusStoreErrorStillRestarts(t *testing.T) {
	f := newRebuildUnitFixture(t, renderedRebuildUnit("ExecStartPre=/bin/true\n"))
	o := f.options(nil)
	o.Store = failingUnitStatusStore{newUnitStatusStore(t)}
	e := NewRebuildServerExecutor(o)
	var buf syncBuffer
	if err := e.Run(context.Background(), &buf, nil); err != nil {
		t.Fatalf("a store error must not fail the run: %v", err)
	}
	if !f.restartIssued(t) {
		t.Fatal("restart not issued")
	}
	if !strings.Contains(buf.String(), "Warning: could not record the systemd unit status: database unavailable") {
		t.Fatalf("missing warning:\n%s", buf.String())
	}
}

func listUnitStatus(t *testing.T, srv *Server) (json.RawMessage, bool) {
	t.Helper()
	admin := NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/maintenance/operations", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srv.handleAdminMaintenanceOps(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	v, ok := body["unitStatus"]
	return v, ok
}

func TestListMaintenanceOperations_UnitStatus(t *testing.T) {
	srv, st := newTestServerWithStore(t)
	ctx := context.Background()

	if v, ok := listUnitStatus(t, srv); ok {
		t.Fatalf("no record: unitStatus must be absent, got %s", v)
	}

	seedDriftRecord(t, st)
	v, ok := listUnitStatus(t, srv)
	if !ok {
		t.Fatal("unitStatus missing with a stored record")
	}
	var got UnitDriftStatus
	if err := json.Unmarshal(v, &got); err != nil || got.Status != "drift" || got.Commit != "old1234" || got.Service != "scion-hub.service" {
		t.Fatalf("unitStatus = %s (err %v)", v, err)
	}

	for _, bad := range []string{`{"status":"bogus"}`, `["drift"]`} {
		if _, err := st.UpsertHubSetting(ctx, HubSettingSectionUnitDrift, json.RawMessage(bad), "system", -1, "managed"); err != nil {
			t.Fatalf("seed %s: %v", bad, err)
		}
		if v, ok := listUnitStatus(t, srv); ok {
			t.Errorf("undecodable record %s: unitStatus must be absent, got %s", bad, v)
		}
	}
}
