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

package handlers

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
)

// futureKeys are top-level keys a newer tool might add to the state file:
// a scalar and a nested object. The values are compact JSON, as the
// writers encode them.
var futureKeys = map[string]string{
	"future_scalar": `42`,
	"future_object": `{"a":[1,{"b":"c"}],"n":null,"s":"x"}`,
}

// addFutureKeys adds futureKeys to the state file, as a newer tool would.
func addFutureKeys(t *testing.T, store *FileSessionState) {
	t.Helper()
	all := readRawState(t, store)
	for k, v := range futureKeys {
		all[k] = json.RawMessage(v)
	}
	data, err := json.Marshal(all)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.Path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// readRawState returns the state file's top-level keys.
func readRawState(t *testing.T, store *FileSessionState) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatalf("reading state: %v", err)
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		t.Fatalf("decoding state: %v", err)
	}
	return all
}

// assertFutureKeys checks that the state file still holds futureKeys,
// unchanged.
func assertFutureKeys(t *testing.T, store *FileSessionState, after string) {
	t.Helper()
	all := readRawState(t, store)
	for k, want := range futureKeys {
		got, ok := all[k]
		if !ok {
			t.Errorf("after %s: key %q dropped", after, k)
			continue
		}
		if !bytes.Equal(got, []byte(want)) {
			t.Errorf("after %s: key %q = %s, want %s", after, k, got, want)
		}
	}
}

// The hook processes' writer (save) keeps unknown keys on every write:
// an event that changes a known field, a session-end that turns the
// session into a pending report, and the claim and confirmation of that
// report by the next hook process.
func TestSessionStateUnknownKeys_HookWriter(t *testing.T) {
	store := NewFileSessionState(t.TempDir())
	openSession(t, store)
	addFutureKeys(t, store)

	// Update, session not ended; a known field changes.
	if got := hookRun(t, store, toolEvent("s1", "Grep")); got != nil {
		t.Fatalf("tool-end ended the session: %+v", *got)
	}
	assertFutureKeys(t, store, "tool-end")
	f := readStateFile(t, store)
	if f.Version != sessionStateVersion || f.Aggregator.SessionID != "s1" || !f.Aggregator.Open ||
		f.Aggregator.ToolCalls["Grep"].Calls != 1 || f.Aggregator.ToolCalls["Bash"].Calls != 1 {
		t.Errorf("after tool-end: state = %+v", f)
	}

	// Update, session ended, by a sender killed before confirming: the
	// session becomes a pending report.
	procs := useFakeProcs(t)
	runKilledSessionEnd(t, store, procs, 200, "s1")
	assertFutureKeys(t, store, "session-end")
	f = readStateFile(t, store)
	if len(f.Pending) != 1 || f.Aggregator.SessionID != "" || f.Aggregator.Open {
		t.Errorf("after session-end: state = %+v", f)
	}

	// modify: the next hook process claims the abandoned report
	// (ClaimAbandonedReports), sends it and confirms it (CompleteReport).
	r := hookRunAll(t, store, toolEvent("", "Read"))
	if len(r) != 1 || r[0].SessionID != "s1" || r[0].ToolCalls["Grep"].Calls != 1 {
		t.Fatalf("reports = %+v", r)
	}
	assertFutureKeys(t, store, "report confirmed")
	f = readStateFile(t, store)
	if len(f.Pending) != 0 {
		t.Errorf("after report confirmed: pending = %+v", f.Pending)
	}
}

// A normal session-end by one hook process (Update to a pending report,
// then CompleteReport) leaves only the unknown keys; the file stays.
func TestSessionStateUnknownKeys_OnlyUnknownKeysNotRemoved(t *testing.T) {
	store := NewFileSessionState(t.TempDir())
	openSession(t, store)
	addFutureKeys(t, store)

	r := hookRunAll(t, store, sessionEvent(hooks.EventSessionEnd, "s1"))
	if len(r) != 1 || r[0].SessionID != "s1" {
		t.Fatalf("reports = %+v", r)
	}
	assertFutureKeys(t, store, "session-end")
	f := readStateFile(t, store)
	if f.Version != sessionStateVersion || len(f.Pending) != 0 || f.Closed ||
		f.Aggregator.SessionID != "" || f.Aggregator.Open {
		t.Errorf("state = %+v", f)
	}

	// The file holding only unknown keys is still usable: the next
	// session counts from zero and keeps the keys.
	hookRun(t, store, sessionEvent(hooks.EventSessionStart, "s2"))
	assertFutureKeys(t, store, "next session-start")
	if f := readStateFile(t, store); f.Aggregator.SessionID != "s2" || !f.Aggregator.Open {
		t.Errorf("after next session-start: state = %+v", f)
	}
}

// The init daemon's writer (writeStateFileInPlace) keeps unknown keys on
// every write: native usage added to the open session, the shutdown
// tombstone, the confirmation of its report, and
// the removal of the tombstone at the next start, which leaves only the
// unknown keys (and would otherwise remove the file).
func TestSessionStateUnknownKeys_InitDaemonWriter(t *testing.T) {
	store := NewFileSessionState(t.TempDir())
	openSession(t, store)
	addFutureKeys(t, store)

	// Native usage added to the open session; a known field changes.
	added, err := store.AddUsage(telemetry.SessionUsage{Calls: 2, TokensInput: 100, TokensOutput: 7})
	if err != nil || !added {
		t.Fatalf("AddUsage = %v, %v", added, err)
	}
	assertFutureKeys(t, store, "usage added")
	f := readStateFile(t, store)
	if f.Closed || f.Aggregator.SessionID != "s1" || !f.Aggregator.Open || f.Aggregator.APICallCount != 2 ||
		f.Aggregator.TokensInput != 100 || f.Aggregator.TokensOutput != 7 || f.Aggregator.ToolCalls["Bash"].Calls != 1 {
		t.Errorf("after usage added: state = %+v", f)
	}

	s, ok, err := store.CloseOpenSession("")
	if err != nil || !ok {
		t.Fatalf("CloseOpenSession = %v, %v", ok, err)
	}
	assertFutureKeys(t, store, "tombstone")
	f = readStateFile(t, store)
	if !f.Closed || f.Aggregator.SessionID != "s1" || f.Aggregator.Open || len(f.Pending) != 1 {
		t.Errorf("after tombstone: state = %+v", f)
	}

	if err := store.CompleteReportsNoFollow(s); err != nil {
		t.Fatalf("CompleteReportsNoFollow: %v", err)
	}
	assertFutureKeys(t, store, "report confirmed")
	if f := readStateFile(t, store); !f.Closed || len(f.Pending) != 0 {
		t.Errorf("after report confirmed: state = %+v", f)
	}

	cleared, err := store.ClearSessionTombstone()
	if err != nil || !cleared {
		t.Fatalf("ClearSessionTombstone = %v, %v", cleared, err)
	}
	assertFutureKeys(t, store, "tombstone cleared")
	f = readStateFile(t, store)
	if f.Version != sessionStateVersion || f.Closed || f.Aggregator.SessionID != "" || len(f.Pending) != 0 {
		t.Errorf("after tombstone cleared: state = %+v", f)
	}
}

// The init daemon's combined shutdown check (close and claim in one pass)
// keeps unknown keys too.
func TestSessionStateUnknownKeys_InitDaemonCloseAndClaim(t *testing.T) {
	procs := useFakeProcs(t)
	store := NewFileSessionState(t.TempDir())
	openSession(t, store)
	runKilledSessionEnd(t, store, procs, 200, "s1")
	// s2 is started by a handler that cannot claim pending reports, so the
	// killed sender's report is left for the shutdown check.
	h := NewTelemetryHandler(nil, nil, nil)
	h.SessionState = killedHook{store}
	if err := h.Handle(sessionEvent(hooks.EventSessionStart, "s2")); err != nil {
		t.Fatal(err)
	}
	addFutureKeys(t, store)

	procs.self = 1
	got, err := store.CloseOpenSessionAndClaimPending("")
	if err != nil {
		t.Fatalf("CloseOpenSessionAndClaimPending: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("shutdown check = %d summaries, want 2 (open s2 and abandoned s1)", len(got))
	}
	assertFutureKeys(t, store, "shutdown check")
	if f := readStateFile(t, store); !f.Closed || f.Aggregator.SessionID != "s2" || len(f.Pending) != 2 {
		t.Errorf("after shutdown check: state = %+v", f)
	}
	if err := store.CompleteReportsNoFollow(got...); err != nil {
		t.Fatalf("CompleteReportsNoFollow: %v", err)
	}
	assertFutureKeys(t, store, "reports confirmed")
}

// A known field wins over an unknown key of the same name, and without
// unknown keys the encoding is exactly the struct's.
func TestEncodeSessionState_KnownFieldsWin(t *testing.T) {
	file := sessionStateFile{
		Aggregator: telemetry.AggregatorState{SessionID: "s1", Open: true},
		extra: map[string]json.RawMessage{
			"version":    json.RawMessage(`99`),
			"closed":     json.RawMessage(`true`),
			"aggregator": json.RawMessage(`{"session_id":"other"}`),
			"pending":    json.RawMessage(`[{}]`),
			"future":     json.RawMessage(`"kept"`),
		},
	}
	data, err := encodeSessionState(file)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeSessionState(data)
	if err != nil {
		t.Fatalf("decoding %s: %v", data, err)
	}
	if got.Version != sessionStateVersion || got.Closed || len(got.Pending) != 0 ||
		got.Aggregator.SessionID != "s1" || !got.Aggregator.Open {
		t.Errorf("known fields overridden: %s", data)
	}
	if len(got.extra) != 1 || string(got.extra["future"]) != `"kept"` {
		t.Errorf("extra = %v, want only future", got.extra)
	}

	file.extra = nil
	data, err = encodeSessionState(file)
	if err != nil {
		t.Fatal(err)
	}
	file.Version = sessionStateVersion
	want, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, want) {
		t.Errorf("without unknown keys:\n got %s\nwant %s", data, want)
	}
}

// encoding/json matches field names case-insensitively, so a key such as
// "Pending" or "Closed" is decoded into the known field. It must not also be
// kept as an unknown key: once the field is cleared, the stale value would
// come back on the next write and be decoded again, re-creating confirmed
// reports or a tombstone that is never cleared. The match follows
// encoding/json's case folding, which goes beyond lower-casing.
func TestSessionStateUnknownKeys_MiscasedKnownKeysNotKept(t *testing.T) {
	// "cloſed" has a long s (U+017F), which encoding/json folds to "s".
	data := []byte(`{"VERSION":1,"cloſed":true,` +
		`"Aggregator":{"session_id":"old"},` +
		`"Pending":[{"summary":{"session_id":"old"}}],` +
		`"future_scalar":42}`)
	file, err := decodeSessionState(data)
	if err != nil {
		t.Fatal(err)
	}
	if file.Version != sessionStateVersion || !file.Closed || len(file.Pending) != 1 ||
		file.Aggregator.SessionID != "old" {
		t.Fatalf("decoded = %+v", file)
	}
	if len(file.extra) != 1 || string(file.extra["future_scalar"]) != `42` {
		t.Errorf("extra = %v, want only future_scalar", file.extra)
	}

	// The tombstone is cleared and the pending report confirmed.
	file.Closed = false
	file.Pending = nil
	file.Aggregator = telemetry.AggregatorState{}
	data, err = encodeSessionState(file)
	if err != nil {
		t.Fatal(err)
	}
	again, err := decodeSessionState(data)
	if err != nil {
		t.Fatalf("decoding %s: %v", data, err)
	}
	if again.Closed || len(again.Pending) != 0 || again.Aggregator.SessionID != "" {
		t.Errorf("cleared fields came back: %s", data)
	}
	if len(again.extra) != 1 || string(again.extra["future_scalar"]) != `42` {
		t.Errorf("extra after round-trip = %v, want only future_scalar", again.extra)
	}

	// The same through a real write path: clearing the tombstone at start.
	store := NewFileSessionState(t.TempDir())
	mustMkdir(t, filepath.Dir(store.Path))
	mustWrite(t, store.Path+".lock", "")
	mustWrite(t, store.Path, `{"version":1,"Closed":true,"aggregator":{"session_id":"old"},"future_scalar":42}`)
	cleared, err := store.ClearSessionTombstone()
	if err != nil || !cleared {
		t.Fatalf("ClearSessionTombstone = %v, %v", cleared, err)
	}
	raw := readRawState(t, store)
	if _, ok := raw["Closed"]; ok {
		t.Errorf("miscased Closed key kept: %v", raw)
	}
	if f := readStateFile(t, store); f.Closed || f.Aggregator.SessionID != "" {
		t.Errorf("tombstone came back: %+v", f)
	}
	if string(raw["future_scalar"]) != `42` {
		t.Errorf("future_scalar = %s, want 42", raw["future_scalar"])
	}
}

// The known keys come from sessionStateFile's json tags. If the derivation
// breaks, this fails loudly; when a field is added, update the list here.
func TestSessionStateKnownKeys_DerivedFromTags(t *testing.T) {
	want := []string{"version", "aggregator", "closed", "pending"}
	if !reflect.DeepEqual(sessionStateKnownKeys, want) {
		t.Errorf("sessionStateKnownKeys = %q, want %q", sessionStateKnownKeys, want)
	}
}

// jsonFieldNames follows encoding/json's rules for top-level names.
func TestJSONFieldNames(t *testing.T) {
	type probe struct {
		Tagged    int    `json:"tagged"`
		OmitEmpty bool   `json:"omit,omitempty"`
		NoName    string `json:",omitempty"`
		Untagged  string
		Skipped   int `json:"-"`
		unexp     int
	}
	_ = probe{}.unexp
	got := jsonFieldNames(reflect.TypeOf(probe{}))
	want := []string{"tagged", "omit", "NoName", "Untagged"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("jsonFieldNames = %q, want %q", got, want)
	}

	// The same names encoding/json writes for a value with every field set.
	data, err := json.Marshal(probe{Tagged: 1, OmitEmpty: true, NoName: "x", Untagged: "y", Skipped: 2})
	if err != nil {
		t.Fatal(err)
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		t.Fatal(err)
	}
	if len(all) != len(want) {
		t.Errorf("encoding/json wrote %s, want keys %q", data, want)
	}
	for _, k := range want {
		if _, ok := all[k]; !ok {
			t.Errorf("encoding/json wrote %s, missing %q", data, k)
		}
	}
}

// Every field's JSON name, whatever fields the struct has now or gains later
// (taken from the struct itself, not from sessionStateKnownKeys), is
// decoded into its field and not kept as an unknown key, in its exact case
// and in upper case. So after the fields are cleared, none of the seeded keys
// comes back on the next write: the output holds only the struct's own
// encoding plus the genuinely unknown key.
func TestSessionStateUnknownKeys_EveryKnownKeyNotKept(t *testing.T) {
	seed := map[string]json.RawMessage{
		"version":       json.RawMessage(`1`),
		"future_scalar": json.RawMessage(`42`),
	}
	for _, k := range jsonFieldNames(reflect.TypeOf(sessionStateFile{})) {
		if k == "version" {
			continue
		}
		// null decodes into a field of any type, leaving it zero.
		seed[k] = json.RawMessage(`null`)
		seed[strings.ToUpper(k)] = json.RawMessage(`null`)
	}
	data, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	file, err := decodeSessionState(data)
	if err != nil {
		t.Fatalf("decoding %s: %v", data, err)
	}
	if len(file.extra) != 1 || string(file.extra["future_scalar"]) != `42` {
		t.Errorf("extra = %v, want only future_scalar", file.extra)
	}

	cleared := sessionStateFile{extra: file.extra}
	out, err := encodeSessionState(cleared)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	plain, err := json.Marshal(sessionStateFile{Version: sessionStateVersion})
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]json.RawMessage
	if err := json.Unmarshal(plain, &want); err != nil {
		t.Fatal(err)
	}
	want["future_scalar"] = json.RawMessage(`42`)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("after clearing:\n got %s\nwant the struct's encoding plus future_scalar", out)
	}
}

// Unknown values are written back without HTML escaping, so a value with
// <, > or & round-trips as read and the file does not grow.
func TestEncodeSessionState_UnknownValuesNotHTMLEscaped(t *testing.T) {
	const value = `{"html":"<a href=\"x\">&amp;</a>","op":"a<b && c>d"}`
	in := []byte(`{"version":1,"aggregator":{"session_id":"s1"},"future":` + value + `}`)
	file, err := decodeSessionState(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := encodeSessionState(file)
	if err != nil {
		t.Fatal(err)
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(out, &all); err != nil {
		t.Fatalf("decoding %s: %v", out, err)
	}
	if string(all["future"]) != value {
		t.Errorf("future = %s, want %s", all["future"], value)
	}
	if bytes.Contains(out, []byte(`\u003c`)) || bytes.Contains(out, []byte(`\u0026`)) {
		t.Errorf("unknown value HTML-escaped: %s", out)
	}
	if bytes.HasSuffix(out, []byte("\n")) {
		t.Errorf("trailing newline in %q", out)
	}

	// A second write of the same file gives the same bytes: nothing grows.
	again, err := decodeSessionState(out)
	if err != nil {
		t.Fatal(err)
	}
	out2, err := encodeSessionState(again)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, out2) {
		t.Errorf("second write differs:\n%s\n%s", out, out2)
	}
}

// jsonFieldNames panics where its names could differ from encoding/json's
// or would be ambiguous, so such a field in sessionStateFile fails at init.
func TestJSONFieldNames_PanicsOnUnsupportedFields(t *testing.T) {
	type Inner struct {
		X int `json:"x"`
	}
	type embedded struct {
		Inner
		A int `json:"a"`
	}
	// The malformed and repeated tags are built at run time: go vet and
	// staticcheck reject them in a literal struct type.
	invalidTag := reflect.StructOf([]reflect.StructField{
		{Name: "A", Type: reflect.TypeOf(0), Tag: reflect.StructTag(`json:"a\\b"`)},
	})
	quoteTag := reflect.StructOf([]reflect.StructField{
		{Name: "A", Type: reflect.TypeOf(0), Tag: reflect.StructTag(`json:"a'b"`)},
	})
	duplicate := reflect.StructOf([]reflect.StructField{
		{Name: "A", Type: reflect.TypeOf(0), Tag: `json:"same"`},
		{Name: "B", Type: reflect.TypeOf(0), Tag: `json:"same"`},
	})
	type foldDuplicate struct {
		A int `json:"closed"`
		B int `json:"Cloſed"`
	}
	type tagDuplicatesFieldName struct {
		Name string
		B    int `json:"name"`
	}
	for _, tc := range []struct {
		name string
		typ  reflect.Type
		want string
	}{
		{"embedded", reflect.TypeOf(embedded{}), "embedded field Inner"},
		{"backslash tag", invalidTag, "encoding/json rejects"},
		{"quote tag", quoteTag, "encoding/json rejects"},
		{"duplicate", duplicate, `duplicates "same"`},
		{"fold duplicate", reflect.TypeOf(foldDuplicate{}), `duplicates "closed"`},
		{"tag duplicates field name", reflect.TypeOf(tagDuplicatesFieldName{}), `duplicates "Name"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("jsonFieldNames(%s) did not panic", tc.typ)
				}
				if msg, _ := r.(string); !strings.Contains(msg, tc.want) {
					t.Errorf("panic = %v, want it to contain %q", r, tc.want)
				}
			}()
			got := jsonFieldNames(tc.typ)
			t.Errorf("jsonFieldNames(%s) = %q", tc.typ, got)
		})
	}
}

// isValidJSONTagName agrees with encoding/json: a tag name it rejects is
// ignored by encoding/json, which uses the field name instead.
func TestIsValidJSONTagName_MatchesEncodingJSON(t *testing.T) {
	for _, name := range []string{"a", "a-b", "a_b", "a.b", "a b", "ä1", "a\\b", "a'b", "a`b", "a\"b", "a,b"} {
		// Build the struct type at run time, with this tag name.
		typ := reflect.StructOf([]reflect.StructField{{
			Name: "Field",
			Type: reflect.TypeOf(0),
			Tag:  reflect.StructTag(`json:"` + strings.ReplaceAll(strings.ReplaceAll(name, `\`, `\\`), `"`, `\"`) + `"`),
		}})
		v := reflect.New(typ).Elem()
		v.Field(0).SetInt(1)
		data, err := json.Marshal(v.Interface())
		if err != nil {
			t.Fatal(err)
		}
		var all map[string]json.RawMessage
		if err := json.Unmarshal(data, &all); err != nil {
			t.Fatal(err)
		}
		tagName, _, _ := strings.Cut(typ.Field(0).Tag.Get("json"), ",")
		valid := isValidJSONTagName(tagName)
		_, usedTag := all[tagName]
		if usedTag != valid {
			t.Errorf("tag name %q: isValidJSONTagName = %v, but encoding/json wrote %s", tagName, valid, data)
		}
	}
}
