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

package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// gcpSpecialKeys are the Cloud Logging fields that must stay top-level.
var gcpSpecialKeys = []string{GCPKeyLabels, GCPKeySourceLocation, "serviceContext"}

func decodeLine(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var data map[string]any
	if err := json.Unmarshal(b, &data); err != nil {
		t.Fatalf("decode %q: %v", b, err)
	}
	return data
}

// assertNoNestedSpecial fails if a special key appears below the top level.
func assertNoNestedSpecial(t *testing.T, path string, v any) {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	for k, child := range m {
		for _, s := range gcpSpecialKeys {
			if k == s {
				t.Errorf("%s nested under %q", s, path)
			}
		}
		assertNoNestedSpecial(t, path+"."+k, child)
	}
}

// TestGCPHandler_GroupsKeepSpecialFieldsTopLevel checks that labels, source
// location and serviceContext stay at the top level whatever groups are open,
// and that user attributes nest exactly as slog's JSON handler nests them
// (ptone/scion#4945).
func TestGCPHandler_GroupsKeepSpecialFieldsTopLevel(t *testing.T) {
	t.Cleanup(func() { SetHubName("") })
	SetHubName("")

	cases := []struct {
		name string
		log  func(l *slog.Logger)
	}{
		{"no group", func(l *slog.Logger) {
			l.With("pre", "x").Info("m", "a", 1)
		}},
		{"group", func(l *slog.Logger) {
			l.WithGroup("g").Info("m", "a", 1)
		}},
		{"attrs before and after group", func(l *slog.Logger) {
			l.With("pre", "x").WithGroup("g").With("post", "y").Info("m", "a", 1)
		}},
		{"nested groups", func(l *slog.Logger) {
			l.With("pre", "x").WithGroup("g").With("p1", "y").
				WithGroup("h").With("p2", "z").Info("m", "a", 1, slog.Group("r", "b", 2))
		}},
		{"empty group", func(l *slog.Logger) {
			l.With("pre", "x").WithGroup("g").Info("m")
		}},
		{"empty inner group", func(l *slog.Logger) {
			l.WithGroup("g").With("p1", "y").WithGroup("h").Info("m")
		}},
		{"zero attr in record", func(l *slog.Logger) {
			l.WithGroup("g").Info("m", slog.Attr{})
		}},
		{"zero attr in outer group", func(l *slog.Logger) {
			l.WithGroup("g").With(slog.Attr{}).WithGroup("h").Info("m")
		}},
		{"empty record group attr", func(l *slog.Logger) {
			l.WithGroup("g").WithGroup("h").Info("m", slog.Group("r"))
		}},
		// User groups passed to With inside a group that render empty,
		// followed by a sibling (review R3).
		{"empty user group in With, zero attr", func(l *slog.Logger) {
			l.WithGroup("g").With("a", 1, slog.Group("r", slog.Attr{})).Info("m", "x", 2)
		}},
		{"empty user group in With, ReplaceAttr", func(l *slog.Logger) {
			l.WithGroup("g").With("a", 1, slog.Group("r", "drop", 1)).Info("m", "x", 2)
		}},
		{"empty nested user group in With", func(l *slog.Logger) {
			l.WithGroup("g").With("a", 1, slog.Group("r", slog.Group("s", "drop", 1))).Info("m", "x", 2)
		}},
		{"partly dropped user group in With", func(l *slog.Logger) {
			l.WithGroup("g").With(slog.Group("r", "drop", 1, "k", 2)).Info("m", "x", 2)
		}},
	}
	// The same caller ReplaceAttr on both handlers: it deletes key "drop".
	dropDrop := func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == "drop" {
			return slog.Attr{}
		}
		return a
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gcpBuf, stdBuf bytes.Buffer
			tc.log(slog.New(NewGCPHandler(&gcpBuf, &slog.HandlerOptions{ReplaceAttr: dropDrop}, "test-component", "hub-a")))
			tc.log(slog.New(slog.NewJSONHandler(&stdBuf, &slog.HandlerOptions{ReplaceAttr: dropDrop})))

			if !json.Valid(bytes.TrimSpace(gcpBuf.Bytes())) {
				t.Fatalf("invalid JSON entry: %s", gcpBuf.Bytes())
			}

			got := decodeLine(t, gcpBuf.Bytes())
			want := decodeLine(t, stdBuf.Bytes())

			for _, k := range gcpSpecialKeys {
				if _, ok := got[k].(map[string]any); !ok {
					t.Errorf("%s missing at top level: %v", k, got)
				}
			}
			labels, _ := got[GCPKeyLabels].(map[string]any)
			if labels["hub"] != "hub-a" || labels["component"] != "test-component" {
				t.Errorf("labels = %v, want hub=hub-a component=test-component", labels)
			}
			assertNoNestedSpecial(t, "", map[string]any{"": stripKeys(got, gcpSpecialKeys...)})

			// User attributes match slog's own nesting.
			gotUser := stripKeys(got, append([]string{GCPKeyTimestamp, GCPKeySeverity, GCPKeyMessage}, gcpSpecialKeys...)...)
			wantUser := stripKeys(want, slog.TimeKey, slog.LevelKey, slog.MessageKey)
			if !reflect.DeepEqual(gotUser, wantUser) {
				t.Errorf("user attrs = %v, want %v", gotUser, wantUser)
			}
		})
	}
}

// topLevelKeys returns the keys of the JSON object in b in order,
// duplicates included, so a test can assert the exact key set.
func topLevelKeys(t *testing.T, b []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("entry %q is not a JSON object: %v %v", b, tok, err)
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("decode key in %q: %v", b, err)
		}
		keys = append(keys, tok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatalf("decode value of %q in %q: %v", tok, b, err)
		}
	}
	return keys
}

// assertKeys fails unless the top-level keys of entry b are exactly want,
// in any order, each once.
func assertKeys(t *testing.T, b []byte, want ...string) {
	t.Helper()
	got := topLevelKeys(t, b)
	sort.Strings(got)
	want = append([]string(nil), want...)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("top-level keys = %v, want %v\nentry: %s", got, want, b)
	}
}

// gcpBaseKeys are the top-level keys of every GCPHandler entry logged
// through a slog.Logger with a message.
var gcpBaseKeys = []string{GCPKeyTimestamp, GCPKeySeverity, GCPKeyMessage, GCPKeyLabels, GCPKeySourceLocation, "serviceContext"}

func withBaseKeys(keys ...string) []string {
	return append(append([]string(nil), gcpBaseKeys...), keys...)
}

// TestGCPHandler_GroupedErrorKeepsSpecialFieldsTopLevel checks the ERROR
// path in a group: special fields top-level, and the Error Reporting
// fields written at the top level too, not in the group (ptone/scion#4996).
func TestGCPHandler_GroupedErrorKeepsSpecialFieldsTopLevel(t *testing.T) {
	var buf bytes.Buffer
	slog.New(NewGCPHandler(&buf, nil, "test-component", "")).WithGroup("g").WithGroup("h").Error("boom", "a", 1)

	assertKeys(t, buf.Bytes(), withBaseKeys("g", gcpKeyStackTrace, gcpKeyType)...)
	data := decodeLine(t, buf.Bytes())
	if data[gcpKeyType] != errorReportingType {
		t.Errorf("@type = %v, want %s", data[gcpKeyType], errorReportingType)
	}
	if st, _ := data[gcpKeyStackTrace].(string); !strings.Contains(st, "goroutine") {
		t.Errorf("stack_trace = %q, want a Go stack trace", st)
	}
	want := map[string]any{"h": map[string]any{"a": float64(1)}}
	if !reflect.DeepEqual(data["g"], want) {
		t.Errorf("g = %v, want %v", data["g"], want)
	}
}

// TestGCPHandler_GroupedCallerErrorFieldsTopLevel checks that stack_trace
// and @type given by the caller inside a group, on the record or with With,
// are written at the top level with the caller's values (ptone/scion#4996).
func TestGCPHandler_GroupedCallerErrorFieldsTopLevel(t *testing.T) {
	cases := []struct {
		name string
		log  func(l *slog.Logger)
	}{
		{"record", func(l *slog.Logger) {
			l.WithGroup("g").Error("boom", "a", 1, gcpKeyStackTrace, "my stack", gcpKeyType, "my type")
		}},
		{"With", func(l *slog.Logger) {
			l.WithGroup("g").With(gcpKeyStackTrace, "my stack").With("a", 1, gcpKeyType, "my type").Error("boom")
		}},
		{"With_INFO", func(l *slog.Logger) {
			l.WithGroup("g").With(gcpKeyStackTrace, "my stack", gcpKeyType, "my type").Info("m", "a", 1)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			tc.log(slog.New(NewGCPHandler(&buf, nil, "test-component", "")))

			assertKeys(t, buf.Bytes(), withBaseKeys(gcpKeyStackTrace, gcpKeyType, "g")...)
			data := decodeLine(t, buf.Bytes())
			if data[gcpKeyStackTrace] != "my stack" || data[gcpKeyType] != "my type" {
				t.Errorf("stack_trace = %v, @type = %v, want caller values", data[gcpKeyStackTrace], data[gcpKeyType])
			}
			if want := map[string]any{"a": float64(1)}; !reflect.DeepEqual(data["g"], want) {
				t.Errorf("g = %v, want %v", data["g"], want)
			}
		})
	}
}

// TestGCPHandler_GroupedTraceIDTopLevel checks that a trace ID attribute
// inside a group becomes the top-level trace field, as it does without a
// group, and leaves no renamed copy in the group (ptone/scion#4996).
func TestGCPHandler_GroupedTraceIDTopLevel(t *testing.T) {
	t.Setenv(EnvGCPProjectID, "test-proj")
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	wantTrace := "projects/test-proj/traces/" + traceID

	cases := []struct {
		name string
		log  func(l *slog.Logger)
	}{
		{"no_group", func(l *slog.Logger) {
			l.Info("m", AttrTraceID, traceID, "a", 1)
		}},
		{"record", func(l *slog.Logger) {
			l.WithGroup("g").Info("m", AttrTraceID, traceID, "a", 1)
		}},
		{"With", func(l *slog.Logger) {
			l.WithGroup("g").With(AttrTraceID, traceID).Info("m", "a", 1)
		}},
		{"nested_groups", func(l *slog.Logger) {
			l.WithGroup("g").With("a", 1).WithGroup("h").Info("m", AttrTraceID, traceID)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			tc.log(slog.New(NewGCPHandler(&buf, nil, "test-component", "")))

			data := decodeLine(t, buf.Bytes())
			userKey := "g"
			if tc.name == "no_group" {
				userKey = "a"
			}
			assertKeys(t, buf.Bytes(), withBaseKeys(GCPKeyTrace, userKey)...)
			if data[GCPKeyTrace] != wantTrace {
				t.Errorf("%s = %v, want %s", GCPKeyTrace, data[GCPKeyTrace], wantTrace)
			}
			labels, _ := data[GCPKeyLabels].(map[string]any)
			if labels[gcpTraceIDLabelKey] != traceID {
				t.Errorf("trace label = %v, want %s", labels[gcpTraceIDLabelKey], traceID)
			}
			if tc.name != "no_group" {
				if want := map[string]any{"a": float64(1)}; !reflect.DeepEqual(data["g"], want) {
					t.Errorf("g = %v, want %v", data["g"], want)
				}
			}
		})
	}
}

// TestGCPHandler_GroupRendersEmptyValidJSON checks that a record whose open
// group renders empty, because its only attrs are written at the top level
// or are deleted by ReplaceAttr, is one valid JSON entry without the group.
func TestGCPHandler_GroupRendersEmptyValidJSON(t *testing.T) {
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	cases := []struct {
		name string
		log  func(l *slog.Logger)
		want []string
	}{
		{"error_only_lifted_record_attr", func(l *slog.Logger) {
			l.WithGroup("g").Error("boom", AttrTraceID, traceID)
		}, withBaseKeys(GCPKeyTrace, gcpKeyStackTrace, gcpKeyType)},
		{"info_only_lifted_With_attr", func(l *slog.Logger) {
			l.WithGroup("g").With(gcpKeyStackTrace, "s").WithGroup("h").Info("m")
		}, withBaseKeys(gcpKeyStackTrace)},
		{"error_only_dropped_attrs", func(l *slog.Logger) {
			l.WithGroup("g").With("drop", 1).Error("boom", "drop", 2)
		}, withBaseKeys(gcpKeyStackTrace, gcpKeyType)},
		{"empty_trace_ID_in_group", func(l *slog.Logger) {
			l.WithGroup("g").Info("m", AttrTraceID, "")
		}, withBaseKeys()},
		// A With stack_trace whose group value renders empty is pruned, so
		// it does not break the separator before the next top-level field,
		// and the generated stack_trace replaces it.
		{"lifted_With_group_renders_empty", func(l *slog.Logger) {
			l.WithGroup("g").With(slog.Group(gcpKeyStackTrace, "drop", 1)).Error("boom", "a", 1)
		}, withBaseKeys(gcpKeyStackTrace, gcpKeyType, "g")},
	}
	dropDrop := func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == "drop" {
			return slog.Attr{}
		}
		return a
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			tc.log(slog.New(NewGCPHandler(&buf, &slog.HandlerOptions{ReplaceAttr: dropDrop}, "test-component", "")))

			if n := bytes.Count(buf.Bytes(), []byte("\n")); n != 1 {
				t.Fatalf("got %d lines, want 1: %s", n, buf.Bytes())
			}
			if !json.Valid(bytes.TrimSpace(buf.Bytes())) {
				t.Fatalf("invalid JSON for an entry whose group renders empty: %s", buf.Bytes())
			}
			assertKeys(t, buf.Bytes(), tc.want...)
		})
	}
}

// TestGCPHandler_GroupSiblingsIndependent checks that handlers derived from
// one grouped handler do not share group attributes.
func TestGCPHandler_GroupSiblingsIndependent(t *testing.T) {
	var buf bytes.Buffer
	base := slog.New(NewGCPHandler(&buf, nil, "test-component", "")).WithGroup("g").With("a", 1)
	b1 := base.With("b", 2)
	b2 := base.With("c", 3)

	for _, tc := range []struct {
		l    *slog.Logger
		want map[string]any
	}{
		{b1, map[string]any{"a": float64(1), "b": float64(2)}},
		{b2, map[string]any{"a": float64(1), "c": float64(3)}},
		{base, map[string]any{"a": float64(1)}},
	} {
		buf.Reset()
		tc.l.Info("m")
		if got := decodeLine(t, buf.Bytes())["g"]; !reflect.DeepEqual(got, tc.want) {
			t.Errorf("g = %v, want %v", got, tc.want)
		}
	}
}

// TestGCPHandler_GroupedDroppedAttrsValidJSON checks that a group whose
// attrs are all deleted by ReplaceAttr (an empty trace_id) still yields one
// valid JSON entry with the special fields top-level.
func TestGCPHandler_GroupedDroppedAttrsValidJSON(t *testing.T) {
	cases := []struct {
		name   string
		log    func(l *slog.Logger)
		wantG  any
		hasKey bool
	}{
		{"record attr dropped", func(l *slog.Logger) {
			l.WithGroup("g").Info("m", AttrTraceID, "")
		}, nil, false},
		{"group attr dropped", func(l *slog.Logger) {
			l.WithGroup("g").With(AttrTraceID, "").Info("m")
		}, nil, false},
		{"inner group dropped", func(l *slog.Logger) {
			l.WithGroup("g").With("a", 1).WithGroup("h").Info("m", AttrTraceID, "")
		}, map[string]any{"a": float64(1)}, true},
		// A trace_id inside a caller's group value is not the trace field:
		// it is kept as is, even when empty (ptone/scion#4996).
		{"user group in With keeps nested trace_id", func(l *slog.Logger) {
			l.WithGroup("g").With("a", 1, slog.Group("r", AttrTraceID, "")).Info("m", "x", 2)
		}, map[string]any{"a": float64(1), "r": map[string]any{AttrTraceID: ""}, "x": float64(2)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			tc.log(slog.New(NewGCPHandler(&buf, nil, "test-component", "")))
			if !json.Valid(bytes.TrimSpace(buf.Bytes())) {
				t.Fatalf("invalid JSON: %s", buf.Bytes())
			}
			data := decodeLine(t, buf.Bytes())
			for _, k := range gcpSpecialKeys {
				if _, ok := data[k].(map[string]any); !ok {
					t.Errorf("%s missing at top level: %v", k, data)
				}
			}
			if g, ok := data["g"]; ok != tc.hasKey || !reflect.DeepEqual(g, tc.wantG) {
				t.Errorf("g = %v (present %v), want %v (present %v)", g, ok, tc.wantG, tc.hasKey)
			}
		})
	}
}

func stripKeys(m map[string]any, keys ...string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	for _, k := range keys {
		delete(out, k)
	}
	return out
}

// TestGCPHandler_ExplicitGroupTraceIDNotRenamed checks that a trace_id
// inside a caller's group value is left as is: not renamed to the trace
// field, and not promoted to the trace field or label (ptone/scion#4996).
func TestGCPHandler_ExplicitGroupTraceIDNotRenamed(t *testing.T) {
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	req := map[string]any{AttrTraceID: traceID}
	cases := []struct {
		name string
		log  func(l *slog.Logger)
		key  string
		want any
	}{
		{"inline_group", func(l *slog.Logger) {
			l.Info("m", slog.Group("req", AttrTraceID, traceID))
		}, "req", req},
		{"inline_group_in_WithGroup", func(l *slog.Logger) {
			l.WithGroup("g").Info("m", slog.Group("req", AttrTraceID, traceID))
		}, "g", map[string]any{"req": req}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			tc.log(slog.New(NewGCPHandler(&buf, nil, "test-component", "")))

			assertKeys(t, buf.Bytes(), withBaseKeys(tc.key)...)
			data := decodeLine(t, buf.Bytes())
			if !reflect.DeepEqual(data[tc.key], tc.want) {
				t.Errorf("explicit group %s = %v, want %v", tc.key, data[tc.key], tc.want)
			}
			labels, _ := data[GCPKeyLabels].(map[string]any)
			if _, ok := labels[gcpTraceIDLabelKey]; ok {
				t.Errorf("trace label set from a nested trace_id: %v", labels)
			}
		})
	}
}

// TestGCPHandler_GroupSiblingsIndependentLifted checks that handlers derived
// from one grouped handler do not share the attrs written at the top level.
// With an aliasing append, the parent's lifted slice would have spare
// capacity after three With calls (cap 4), so the second sibling would
// overwrite the first. The real code allocates exact capacity.
func TestGCPHandler_GroupSiblingsIndependentLifted(t *testing.T) {
	t.Setenv(EnvGCPProjectID, "test-proj")
	const idA = "4bf92f3577b34da6a3ce929d0e0e4736"
	const idB = "0af7651916cd43dd8448eb211c80319c"
	var buf bytes.Buffer
	base := slog.New(NewGCPHandler(&buf, nil, "test-component", "")).WithGroup("g").
		With(gcpKeyStackTrace, "s1").With(gcpKeyStackTrace, "s2").With(gcpKeyType, "t")
	a := base.With(AttrTraceID, idA)
	b := base.With(AttrTraceID, idB)

	for _, tc := range []struct {
		name string
		l    *slog.Logger
		want string
	}{
		{"a", a, idA},
		{"b", b, idB},
	} {
		buf.Reset()
		tc.l.Info("m")
		assertKeys(t, buf.Bytes(), withBaseKeys(gcpKeyStackTrace, gcpKeyType, GCPKeyTrace)...)
		data := decodeLine(t, buf.Bytes())
		if got, want := data[GCPKeyTrace], "projects/test-proj/traces/"+tc.want; got != want {
			t.Errorf("sibling %s: trace field = %v, want %s", tc.name, got, want)
		}
		if data[gcpKeyStackTrace] != "s2" || data[gcpKeyType] != "t" {
			t.Errorf("sibling %s: stack_trace = %v, @type = %v, want s2, t", tc.name, data[gcpKeyStackTrace], data[gcpKeyType])
		}
	}
	buf.Reset()
	base.Info("m")
	assertKeys(t, buf.Bytes(), withBaseKeys(gcpKeyStackTrace, gcpKeyType)...)
}

// TestGCPHandler_LiftedFieldOnePerKey checks that stack_trace, @type and
// trace_id given more than once, by With (before or after WithGroup) and on
// the record, are written once each at the top level, the record's value
// winning over With and a later With over an earlier one (ptone/scion#4998).
func TestGCPHandler_LiftedFieldOnePerKey(t *testing.T) {
	t.Setenv(EnvGCPProjectID, "test-proj")
	const idA = "4bf92f3577b34da6a3ce929d0e0e4736"
	const idB = "0af7651916cd43dd8448eb211c80319c"
	cases := []struct {
		name  string
		log   func(l *slog.Logger)
		keys  []string
		trace string // want trace ID, "" = no trace field
		st    string // want stack_trace, "" = not checked
	}{
		{"With_and_record", func(l *slog.Logger) {
			l.With(AttrTraceID, idA).Info("m", AttrTraceID, idB)
		}, []string{GCPKeyTrace}, idB, ""},
		{"With_before_group_and_record", func(l *slog.Logger) {
			l.With(AttrTraceID, idA).WithGroup("g").Info("m", AttrTraceID, idB, "a", 1)
		}, []string{GCPKeyTrace, "g"}, idB, ""},
		{"With_in_group_and_record", func(l *slog.Logger) {
			l.WithGroup("g").With(AttrTraceID, idA).Info("m", AttrTraceID, idB)
		}, []string{GCPKeyTrace}, idB, ""},
		{"With_before_and_in_group", func(l *slog.Logger) {
			l.With(AttrTraceID, idA).WithGroup("g").With(AttrTraceID, idB).Info("m")
		}, []string{GCPKeyTrace}, idB, ""},
		{"error_fields_With_and_record", func(l *slog.Logger) {
			l.With(gcpKeyStackTrace, "s1", gcpKeyType, "t1").WithGroup("g").
				Error("boom", gcpKeyStackTrace, "s2", gcpKeyType, "t2")
		}, []string{gcpKeyStackTrace, gcpKeyType}, "", "s2"},
		{"error_fields_ungrouped", func(l *slog.Logger) {
			l.With(gcpKeyStackTrace, "s1").Error("boom", gcpKeyStackTrace, "s2")
		}, []string{gcpKeyStackTrace, gcpKeyType}, "", "s2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			tc.log(slog.New(NewGCPHandler(&buf, nil, "test-component", "")))

			assertKeys(t, buf.Bytes(), withBaseKeys(tc.keys...)...)
			data := decodeLine(t, buf.Bytes())
			if tc.trace != "" {
				if got, want := data[GCPKeyTrace], "projects/test-proj/traces/"+tc.trace; got != want {
					t.Errorf("one-per-key trace field = %v, want %s", got, want)
				}
				labels, _ := data[GCPKeyLabels].(map[string]any)
				if labels[gcpTraceIDLabelKey] != tc.trace {
					t.Errorf("trace label = %v, want %s", labels[gcpTraceIDLabelKey], tc.trace)
				}
			}
			if tc.st != "" && data[gcpKeyStackTrace] != tc.st {
				t.Errorf("one-per-key stack_trace = %v, want %s", data[gcpKeyStackTrace], tc.st)
			}
		})
	}
}

// traceValuer is a LogValuer for a trace ID. As a struct, it does not
// format as its trace ID, so only its resolved value carries the ID.
type traceValuer struct{ id string }

func (v traceValuer) LogValue() slog.Value { return slog.StringValue(v.id) }

// emptyTraceCase is one entry logged with trace_ids from With and the
// record, and the entry wanted: top lists its top-level keys besides
// gcpBaseKeys, and trace is the trace ID of both the trace field and the
// trace label, "" meaning neither is present.
type emptyTraceCase struct {
	name  string
	log   func(l *slog.Logger)
	top   []string
	trace string
}

// runEmptyTraceCases logs each case and checks the line's exact top-level
// keys and that the trace field and the trace label agree with tc.trace.
func runEmptyTraceCases(t *testing.T, cases []emptyTraceCase) {
	t.Helper()
	t.Setenv(EnvGCPProjectID, "test-proj")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			tc.log(slog.New(NewGCPHandler(&buf, nil, "test-component", "")))

			assertKeys(t, buf.Bytes(), withBaseKeys(tc.top...)...)
			data := decodeLine(t, buf.Bytes())
			field, hasField := data[GCPKeyTrace]
			labels, _ := data[GCPKeyLabels].(map[string]any)
			label, hasLabel := labels[gcpTraceIDLabelKey]
			if tc.trace == "" {
				if hasField || hasLabel {
					t.Errorf("trace field = %v (present %v), label = %v (present %v), want neither", field, hasField, label, hasLabel)
				}
				return
			}
			if want := "projects/test-proj/traces/" + tc.trace; field != want {
				t.Errorf("trace field = %v, want %s", field, want)
			}
			if label != tc.trace {
				t.Errorf("trace label = %v, want %s", label, tc.trace)
			}
		})
	}
}

// TestGCPHandler_EmptyTraceIDWithNeverOverrides checks that a trace_id
// from a later With that normalizes to empty does not override an earlier
// one, for the trace field or the label (ptone/scion#5062).
func TestGCPHandler_EmptyTraceIDWithNeverOverrides(t *testing.T) {
	const idA = "4bf92f3577b34da6a3ce929d0e0e4736"
	const idB = "0af7651916cd43dd8448eb211c80319c"
	runEmptyTraceCases(t, []emptyTraceCase{
		{"empty", func(l *slog.Logger) {
			l.With(AttrTraceID, idA).With(AttrTraceID, "").Info("m")
		}, []string{GCPKeyTrace}, idA},
		{"invalid", func(l *slog.Logger) {
			l.With(AttrTraceID, idA).With(AttrTraceID, "not-a-trace").Info("m")
		}, []string{GCPKeyTrace}, idA},
		{"group_value", func(l *slog.Logger) {
			l.With(AttrTraceID, idA).With(slog.Group(AttrTraceID, "x", 1)).Info("m")
		}, []string{GCPKeyTrace}, idA},
		{"empty_in_group", func(l *slog.Logger) {
			l.WithGroup("g").With(AttrTraceID, idA).With(AttrTraceID, "").Info("m", "a", 1)
		}, []string{GCPKeyTrace, "g"}, idA},
		{"empty_LogValuer", func(l *slog.Logger) {
			l.With(AttrTraceID, idA).With(AttrTraceID, traceValuer{""}).Info("m")
		}, []string{GCPKeyTrace}, idA},
		{"valid_LogValuer_overrides", func(l *slog.Logger) {
			l.With(AttrTraceID, idA).With(AttrTraceID, traceValuer{idB}).Info("m")
		}, []string{GCPKeyTrace}, idB},
		{"empty_only", func(l *slog.Logger) {
			l.With(AttrTraceID, "").Info("m")
		}, nil, ""},
	})
}

// TestGCPHandler_EmptyTraceIDRecordNeverOverrides checks that a record
// trace_id that normalizes to empty does not override one from With, for
// the trace field or the label (ptone/scion#5062).
func TestGCPHandler_EmptyTraceIDRecordNeverOverrides(t *testing.T) {
	const idA = "4bf92f3577b34da6a3ce929d0e0e4736"
	runEmptyTraceCases(t, []emptyTraceCase{
		{"empty", func(l *slog.Logger) {
			l.With(AttrTraceID, idA).Info("m", AttrTraceID, "")
		}, []string{GCPKeyTrace}, idA},
		{"invalid", func(l *slog.Logger) {
			l.With(AttrTraceID, idA).Info("m", AttrTraceID, "not-a-trace")
		}, []string{GCPKeyTrace}, idA},
		{"group_value", func(l *slog.Logger) {
			l.With(AttrTraceID, idA).Info("m", slog.Group(AttrTraceID, "x", 1))
		}, []string{GCPKeyTrace}, idA},
		{"empty_in_group", func(l *slog.Logger) {
			l.With(AttrTraceID, idA).WithGroup("g").Info("m", AttrTraceID, "", "a", 1)
		}, []string{GCPKeyTrace, "g"}, idA},
		{"empty_LogValuer", func(l *slog.Logger) {
			l.With(AttrTraceID, idA).Info("m", AttrTraceID, traceValuer{""})
		}, []string{GCPKeyTrace}, idA},
		{"empty_only", func(l *slog.Logger) {
			l.Info("m", AttrTraceID, "")
		}, nil, ""},
	})
}

// TestGCPHandler_TraceLabelMatchesField checks that the trace label is
// taken from the trace_id written as the trace field, whichever of With
// and the record it comes from, including a LogValuer's resolved value
// (ptone/scion#5062).
func TestGCPHandler_TraceLabelMatchesField(t *testing.T) {
	const idA = "4bf92f3577b34da6a3ce929d0e0e4736"
	const idB = "0af7651916cd43dd8448eb211c80319c"
	runEmptyTraceCases(t, []emptyTraceCase{
		{"record_over_empty_With", func(l *slog.Logger) {
			l.With(AttrTraceID, "").Info("m", AttrTraceID, idB)
		}, []string{GCPKeyTrace}, idB},
		{"record_LogValuer", func(l *slog.Logger) {
			l.With(AttrTraceID, idA).Info("m", AttrTraceID, traceValuer{idB})
		}, []string{GCPKeyTrace}, idB},
		{"With_LogValuer", func(l *slog.Logger) {
			l.With(AttrTraceID, traceValuer{idB}).Info("m")
		}, []string{GCPKeyTrace}, idB},
		{"With_LogValuer_in_group", func(l *slog.Logger) {
			l.WithGroup("g").With(AttrTraceID, traceValuer{idB}).Info("m")
		}, []string{GCPKeyTrace}, idB},
		{"empty_record_and_With", func(l *slog.Logger) {
			l.With(AttrTraceID, " ").Info("m", AttrTraceID, "")
		}, nil, ""},
	})
}
