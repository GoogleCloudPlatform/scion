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
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"time"
)

// testAdminAuditTag prefixes every line the test admin audit sink writes, so
// the lines can be selected from the process output (journald on systemd
// test hubs) without parsing anything else.
const testAdminAuditTag = "[scion-test-admin-audit]"

// Test admin audit event names.
const (
	testAdminAuditEventGateStart = "test_admin_gate_start"
	testAdminAuditEventBrokerSet = "test_admin_broker_set"
)

// testAdminAuditOutput is where the sink writes: the process stdout. A
// package variable only so tests can capture the output.
var testAdminAuditOutput io.Writer = os.Stdout

// testAdminAuditSink is the synchronous, level-independent writer for test
// admin tier events (admin tier design §3.6.2). It deliberately does not
// use slog (whose level server.log_level controls at runtime), the async
// decision-audit writer, or any experiment: each event is one JSON line,
// written under a fixed tag directly to the output before emit returns.
//
// Phase A0 emits only the gate-start event (the startup banner). Phase A2
// extends this type into the hash-chained testAdminAuditSink (issuance,
// per-request, cage denial, revoke-all and purge events, fail-closed).
type testAdminAuditSink struct {
	mu  sync.Mutex
	out io.Writer
	now func() time.Time
}

func newTestAdminAuditSink(out io.Writer) *testAdminAuditSink {
	return &testAdminAuditSink{out: out, now: time.Now}
}

// emit writes one tagged JSON line for event with the given fields. The
// "event" and "ts" fields are set by the sink. It returns the write error,
// if any; callers fail closed on it.
func (k *testAdminAuditSink) emit(event string, fields map[string]any) error {
	if k == nil || k.out == nil {
		return errors.New("test admin audit sink is not configured")
	}
	rec := make(map[string]any, len(fields)+2)
	for key, v := range fields {
		rec[key] = v
	}
	rec["event"] = event
	rec["ts"] = k.now().UTC().Format(time.RFC3339Nano)
	body, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	line := make([]byte, 0, len(testAdminAuditTag)+1+len(body)+1)
	line = append(line, testAdminAuditTag...)
	line = append(line, ' ')
	line = append(line, body...)
	line = append(line, '\n')

	k.mu.Lock()
	defer k.mu.Unlock()
	_, err = k.out.Write(line)
	return err
}
