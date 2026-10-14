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

// TestGCPHandler_GroupedErrorKeepsSpecialFieldsTopLevel checks the ERROR
// path in a group: special fields top-level, Error Reporting fields still
// written.
func TestGCPHandler_GroupedErrorKeepsSpecialFieldsTopLevel(t *testing.T) {
	var buf bytes.Buffer
	slog.New(NewGCPHandler(&buf, nil, "test-component", "")).WithGroup("g").Error("boom", "a", 1)

	data := decodeLine(t, buf.Bytes())
	for _, k := range gcpSpecialKeys {
		if _, ok := data[k].(map[string]any); !ok {
			t.Errorf("%s missing at top level: %v", k, data)
		}
	}
	g, ok := data["g"].(map[string]any)
	if !ok {
		t.Fatalf("group g missing: %v", data)
	}
	if g["a"] != float64(1) || g["@type"] != errorReportingType || g["stack_trace"] == nil {
		t.Errorf("group g = %v, want a, @type and stack_trace", g)
	}
	assertNoNestedSpecial(t, "g", g)
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
