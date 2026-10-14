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
				t.Fatalf("invalid JSON entry: %s", buf.Bytes())
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
		{"user group in With dropped", func(l *slog.Logger) {
			l.WithGroup("g").With("a", 1, slog.Group("r", AttrTraceID, "")).Info("m", "x", 2)
		}, map[string]any{"a": float64(1), "x": float64(2)}, true},
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
