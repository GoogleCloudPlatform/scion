/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	state "github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/dialects"
	"github.com/GoogleCloudPlatform/scion/pkg/testutil"
	"gopkg.in/yaml.v3"
)

// loadPiBridgePayloads reads
// pkg/sciontool/hooks/dialects/testdata/pi/bridge-payloads.jsonl: the exact
// stdin harnesses/pi's scion-bridge.js writes to `sciontool hook
// --dialect=pi` for a scripted session. harnesses/pi/scion-bridge.test.mjs
// asserts the bridge still produces this file byte for byte, so these are
// the bridge's real payload shapes (a tool_input object, a boolean success,
// an error string). The pi events fed to the bridge are synthetic, not a
// live pi capture.
func loadPiBridgePayloads(t *testing.T) []map[string]interface{} {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../../../.."))
	f, err := os.Open(filepath.Join(root, "pkg", "sciontool", "hooks", "dialects", "testdata", "pi", "bridge-payloads.jsonl"))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer func() { _ = f.Close() }()

	var records []map[string]interface{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if scanner.Text() == "" {
			continue
		}
		var rec map[string]interface{}
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			t.Fatalf("fixture line %d: %v", len(records)+1, err)
		}
		records = append(records, rec)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return records
}

// TestPiDialect_BridgePayloads feeds the bridge's payloads through the real
// harnesses/pi/dialect.yaml and pins the normalized event, the fields the
// handlers read, readiness (and the in-place classification of a
// session-start) and the resulting status activity.
func TestPiDialect_BridgePayloads(t *testing.T) {
	specPath := testutil.HarnessDialectPath(t, "pi")
	md, err := dialects.LoadMappingDialect(specPath)
	if err != nil {
		t.Fatalf("LoadMappingDialect(%s): %v", specPath, err)
	}
	if md.Name() != "pi" {
		t.Fatalf("dialect name = %q, want pi", md.Name())
	}

	type want struct {
		name      string
		toolName  string
		toolInput map[string]interface{} // nil: ToolInput must be empty
		success   bool
		// failed: EventData.Failed, set by the mapping from success
		// (false -> true); only record 5 reports a failed tool call.
		failed bool
		errMsg string
		source string
		// inPlace: a session-start that restarts the session inside the
		// running harness (hooks.SessionInPlace): not a readiness event.
		inPlace  bool
		activity state.Activity
	}
	wants := []want{
		{name: hooks.EventSessionStart, source: "startup", activity: state.ActivityWorking},
		{name: hooks.EventPromptSubmit, activity: state.ActivityThinking},
		{name: hooks.EventToolStart, toolName: "bash", toolInput: map[string]interface{}{"command": "ls -la", "timeout": float64(30)}, activity: state.ActivityExecuting},
		{name: hooks.EventToolEnd, toolName: "bash", success: true, activity: state.ActivityWorking},
		{name: hooks.EventToolStart, toolName: "read", toolInput: map[string]interface{}{"path": "missing.txt"}, activity: state.ActivityExecuting},
		{name: hooks.EventToolEnd, toolName: "read", success: false, failed: true, activity: state.ActivityWorking},
		{name: hooks.EventAgentEnd, activity: state.ActivityWorking},
		{name: hooks.EventPromptSubmit, activity: state.ActivityThinking},
		{name: hooks.EventAgentEnd, errMsg: "429 rate limited: retry later", activity: state.ActivityWorking},
		{name: hooks.EventSessionStart, source: "resume", activity: state.ActivityWorking},
		// pi's /reload restarts the session in place: no readiness
		// effect, and the status handler releases only a
		// notification-set wait (an agent-set status is kept). The
		// activity is what eventToPhaseActivity maps a session-start to;
		// the handler routes an in-place start around it.
		{name: hooks.EventSessionStart, source: "reload", inPlace: true, activity: state.ActivityWorking},
	}

	records := loadPiBridgePayloads(t)
	if len(records) != len(wants) {
		t.Fatalf("fixture has %d records, want %d", len(records), len(wants))
	}
	for i, rec := range records {
		w := wants[i]
		ev, err := md.Parse(rec)
		if err != nil {
			t.Fatalf("record %d: Parse: %v", i, err)
		}
		if ev.Name != w.name {
			t.Errorf("record %d (%v): Name = %q, want %q", i, rec["hook_event_name"], ev.Name, w.name)
		}
		if ev.Data.ToolName != w.toolName {
			t.Errorf("record %d: ToolName = %q, want %q", i, ev.Data.ToolName, w.toolName)
		}
		if w.toolInput == nil {
			if ev.Data.ToolInput != "" {
				t.Errorf("record %d: ToolInput = %q, want empty", i, ev.Data.ToolInput)
			}
		} else {
			// The bridge sends tool_input as a JSON object; the dialect
			// must carry it through as JSON text, not drop it.
			var got map[string]interface{}
			if err := json.Unmarshal([]byte(ev.Data.ToolInput), &got); err != nil {
				t.Errorf("record %d: ToolInput %q is not JSON: %v", i, ev.Data.ToolInput, err)
			} else if !jsonEqual(got, w.toolInput) {
				t.Errorf("record %d: ToolInput = %v, want %v", i, got, w.toolInput)
			}
		}
		if ev.Data.Success != w.success {
			t.Errorf("record %d: Success = %v, want %v", i, ev.Data.Success, w.success)
		}
		if ev.Data.Failed != w.failed {
			t.Errorf("record %d: Failed = %v, want %v", i, ev.Data.Failed, w.failed)
		}
		if ev.Data.Source != w.source {
			t.Errorf("record %d: Source = %q, want %q", i, ev.Data.Source, w.source)
		}
		if ev.Data.Error != w.errMsg {
			t.Errorf("record %d: Error = %q, want %q", i, ev.Data.Error, w.errMsg)
		}
		if ev.Name == hooks.EventSessionStart {
			if got := hooks.ClassifySessionStart(ev.Data.Source) == hooks.SessionInPlace; got != w.inPlace {
				t.Errorf("record %d: source %q in place = %v, want %v", i, ev.Data.Source, got, w.inPlace)
			}
		}
		if got := hooks.IsReadinessEvent(ev); got != !w.inPlace {
			t.Errorf("record %d: %s readiness event = %v, want %v", i, ev.Name, got, !w.inPlace)
		}
		res := eventToPhaseActivity(ev)
		if res == nil {
			t.Errorf("record %d: %s maps to no status change", i, ev.Name)
		} else if res.activity != w.activity {
			t.Errorf("record %d: activity = %q, want %q", i, res.activity, w.activity)
		}
	}
}

// TestPiDialect_FixtureCoversEveryMapping fails when dialect.yaml gains a
// mapping the bridge fixture never exercises, or the fixture carries an
// event dialect.yaml does not map.
func TestPiDialect_FixtureCoversEveryMapping(t *testing.T) {
	raw, err := os.ReadFile(testutil.HarnessDialectPath(t, "pi"))
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Mappings map[string]interface{} `yaml:"mappings"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for i, rec := range loadPiBridgePayloads(t) {
		name, _ := rec["hook_event_name"].(string)
		if _, ok := spec.Mappings[name]; !ok {
			t.Errorf("record %d: event %q is not mapped in dialect.yaml", i, name)
		}
		seen[name] = true
	}
	for name := range spec.Mappings {
		if !seen[name] {
			t.Errorf("dialect.yaml maps %q but the bridge fixture never emits it", name)
		}
	}
}

func jsonEqual(a, b interface{}) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(ja) == string(jb)
}
