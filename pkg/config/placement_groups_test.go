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

package config

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// GT1: LiteralSiblingGroups groups entries with equal explicit contexts or
// both empty, and equal or both-empty namespaces; others do not group.
func TestLiteralSiblingGroups_GT1(t *testing.T) {
	runtimes := map[string]V1RuntimeConfig{
		"a1":     {Type: "kubernetes", Context: "ctx-a", Namespace: "ns"},
		"a2":     {Type: "k8s", Context: "ctx-a", Namespace: "ns"},
		"a3":     {Type: "kubernetes", Context: "ctx-a", Namespace: "other"},
		"a4":     {Type: "kubernetes", Context: "ctx-a"},
		"e1":     {Type: "kubernetes"},
		"e2":     {Type: "remote"},
		"e3":     {Type: "kubernetes", Namespace: "ns"},
		"b1":     {Type: "kubernetes", Context: "ctx-b", Namespace: "ns"},
		"docker": {Type: "docker", Context: "ctx-a", Namespace: "ns"},
		"k8s":    {}, // no type: the name makes it a Kubernetes entry
	}
	got := LiteralSiblingGroups(runtimes)
	want := [][]string{{"a1", "a2"}, {"e1", "e2", "k8s"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LiteralSiblingGroups = %v, want %v", got, want)
	}
	if got := LiteralSiblingGroups(nil); got != nil {
		t.Fatalf("LiteralSiblingGroups(nil) = %v, want nil", got)
	}
}

func hostContext(ctx string, conn KubeConnection, err error, calls *int) func() (string, KubeConnection, error) {
	return func() (string, KubeConnection, error) {
		*calls++
		return ctx, conn, err
	}
}

// GT2: IdentityGroups resolves kubeconfig, in-cluster and namespace
// defaults; a callback error groups the affected entry with every
// same-namespace entry, with a warning.
func TestIdentityGroups_GT2(t *testing.T) {
	t.Run("kubeconfig resolution", func(t *testing.T) {
		runtimes := map[string]V1RuntimeConfig{
			"empty":    {Type: "kubernetes", Namespace: "ns"},
			"explicit": {Type: "kubernetes", Context: "ctx-a", Namespace: "ns"},
			"other":    {Type: "kubernetes", Context: "ctx-b", Namespace: "ns"},
			"defns":    {Type: "kubernetes", Context: "ctx-a"}, // namespace from defaultNS
			"otherns":  {Type: "kubernetes", Context: "ctx-a", Namespace: "x"},
			"docker":   {Type: "docker", Namespace: "ns"},
		}
		calls := 0
		groups, warnings := IdentityGroups(runtimes, hostContext("ctx-a", KubeConnKubeconfig, nil, &calls), "ns")
		want := [][]string{{"defns", "empty", "explicit"}}
		if !reflect.DeepEqual(groups, want) || warnings != nil {
			t.Fatalf("IdentityGroups = %v, %v; want %v, no warnings", groups, warnings, want)
		}
		if calls != 1 {
			t.Fatalf("callback calls = %d, want 1", calls)
		}
	})
	t.Run("in-cluster resolution", func(t *testing.T) {
		runtimes := map[string]V1RuntimeConfig{
			"e1":       {Type: "kubernetes", Namespace: "pod-ns"},
			"e2":       {Type: "kubernetes"}, // namespace from defaultNS
			"e3":       {Type: "kubernetes", Namespace: "other"},
			"explicit": {Type: "kubernetes", Context: "in-cluster", Namespace: "pod-ns"},
		}
		calls := 0
		groups, warnings := IdentityGroups(runtimes, hostContext("ignored", KubeConnInCluster, nil, &calls), "pod-ns")
		want := [][]string{{"e1", "e2"}}
		if !reflect.DeepEqual(groups, want) || warnings != nil {
			t.Fatalf("IdentityGroups = %v, %v; want %v, no warnings", groups, warnings, want)
		}
		if calls != 1 {
			t.Fatalf("callback calls = %d, want 1", calls)
		}
	})
	t.Run("no empty-context entry does not call back", func(t *testing.T) {
		runtimes := map[string]V1RuntimeConfig{
			"a": {Type: "kubernetes", Context: "ctx-a", Namespace: "ns"},
			"b": {Type: "kubernetes", Context: "ctx-a", Namespace: "ns"},
		}
		groups, warnings := IdentityGroups(runtimes, func() (string, KubeConnection, error) {
			t.Fatal("callback must not be invoked")
			return "", KubeConnUnknown, nil
		}, "default")
		if !reflect.DeepEqual(groups, [][]string{{"a", "b"}}) || warnings != nil {
			t.Fatalf("IdentityGroups = %v, %v", groups, warnings)
		}
	})
	for _, tc := range []struct {
		name string
		ctx  string
		conn KubeConnection
		err  error
	}{
		{"callback error", "", KubeConnKubeconfig, errors.New("kubeconfig unreadable")},
		{"unknown connection", "", KubeConnUnknown, nil},
		{"kubeconfig without current context", "", KubeConnKubeconfig, nil},
	} {
		t.Run("fail closed: "+tc.name, func(t *testing.T) {
			runtimes := map[string]V1RuntimeConfig{
				"u1": {Type: "kubernetes", Namespace: "ns"},
				"u2": {Type: "kubernetes"}, // namespace from defaultNS
				"a":  {Type: "kubernetes", Context: "ctx-a", Namespace: "ns"},
				"b":  {Type: "kubernetes", Context: "ctx-b", Namespace: "ns"},
				"x":  {Type: "kubernetes", Context: "ctx-a", Namespace: "x"},
				"u3": {Type: "kubernetes", Namespace: "y"}, // alone in its namespace
			}
			calls := 0
			groups, warnings := IdentityGroups(runtimes, hostContext(tc.ctx, tc.conn, tc.err, &calls), "ns")
			// u1 and u2 each group with every entry in namespace ns (the same
			// set, reported once); a and b do not group with each other
			// alone, so groups overlap only through the unresolved entries.
			want := [][]string{{"a", "b", "u1", "u2"}}
			if !reflect.DeepEqual(groups, want) {
				t.Fatalf("groups = %v, want %v", groups, want)
			}
			if calls != 1 {
				t.Fatalf("callback calls = %d, want 1", calls)
			}
			if len(warnings) != 3 {
				t.Fatalf("warnings = %v, want one per unresolved entry (u1, u2, u3)", warnings)
			}
			for i, name := range []string{"u1", "u2", "u3"} {
				if !strings.HasPrefix(warnings[i], "runtimes."+name+":") {
					t.Fatalf("warnings[%d] = %q, want it to name runtimes.%s", i, warnings[i], name)
				}
			}
		})
	}
	t.Run("callback error text stays out of the warning", func(t *testing.T) {
		runtimes := map[string]V1RuntimeConfig{
			"u": {Type: "kubernetes", Namespace: "ns"},
			"a": {Type: "kubernetes", Context: "ctx-a", Namespace: "ns"},
		}
		const detail = "open /distinctive/path/kubeconfig-7f3a: permission denied"
		callbackErr := errors.New(detail)
		capture := capturePlacementSlog(t)
		calls := 0
		groups, warnings := IdentityGroups(runtimes, hostContext("", KubeConnKubeconfig, callbackErr, &calls), "ns")
		records := capture.snapshot()
		if len(records) != 1 {
			t.Fatalf("slog records = %+v, want exactly one", records)
		}
		rec := records[0]
		if rec.level != slog.LevelWarn {
			t.Fatalf("slog level = %v, want WARN", rec.level)
		}
		if rec.msg != "Kubernetes placement: could not read the current context to group runtime entries" {
			t.Fatalf("slog message = %q", rec.msg)
		}
		logged, ok := rec.attrs["error"]
		if !ok {
			t.Fatalf("slog attrs = %v, want an error attribute", rec.attrs)
		}
		if loggedErr, isErr := logged.Any().(error); !isErr || loggedErr != callbackErr {
			t.Fatalf("slog error attribute = %v, want the callback error", logged)
		}
		if !reflect.DeepEqual(groups, [][]string{{"a", "u"}}) {
			t.Fatalf("groups = %v, want [[a u]]", groups)
		}
		want := `runtimes.u: cluster identity could not be resolved (the current context could not be read); treating it as targeting the same cluster as every entry in namespace "ns"`
		if len(warnings) != 1 || warnings[0] != want {
			t.Fatalf("warnings = %q, want [%q]", warnings, want)
		}
		if strings.Contains(warnings[0], "distinctive") || strings.Contains(warnings[0], "permission denied") {
			t.Fatalf("warning carries the callback error text: %q", warnings[0])
		}
	})
	t.Run("overlapping groups", func(t *testing.T) {
		runtimes := map[string]V1RuntimeConfig{
			"u": {Type: "kubernetes", Namespace: "ns"},
			"a": {Type: "kubernetes", Context: "ctx-a", Namespace: "ns"},
			"c": {Type: "kubernetes", Context: "ctx-a", Namespace: "ns"},
			"b": {Type: "kubernetes", Context: "ctx-b", Namespace: "ns"},
		}
		calls := 0
		groups, warnings := IdentityGroups(runtimes, hostContext("", KubeConnUnknown, nil, &calls), "ns")
		want := [][]string{{"a", "b", "c", "u"}, {"a", "c"}}
		if !reflect.DeepEqual(groups, want) || len(warnings) != 1 {
			t.Fatalf("IdentityGroups = %v, %v; want %v and one warning", groups, warnings, want)
		}
	})
}

// placementLogRecord is one captured slog record.
type placementLogRecord struct {
	level slog.Level
	msg   string
	attrs map[string]slog.Value
}

// placementLogCapture is a slog.Handler that keeps every record it handles.
type placementLogCapture struct {
	mu      sync.Mutex
	records []placementLogRecord
}

func (c *placementLogCapture) Enabled(context.Context, slog.Level) bool { return true }

func (c *placementLogCapture) Handle(_ context.Context, r slog.Record) error {
	rec := placementLogRecord{level: r.Level, msg: r.Message, attrs: map[string]slog.Value{}}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value
		return true
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, rec)
	return nil
}

func (c *placementLogCapture) WithAttrs([]slog.Attr) slog.Handler { return c }

func (c *placementLogCapture) WithGroup(string) slog.Handler { return c }

func (c *placementLogCapture) snapshot() []placementLogRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]placementLogRecord(nil), c.records...)
}

// capturePlacementSlog installs a capturing default slog logger for the
// test and restores the previous logger, and the log package output that
// slog.SetDefault redirects, on cleanup. It swaps process-wide state, so a
// test that calls it must not use t.Parallel().
func capturePlacementSlog(t *testing.T) *placementLogCapture {
	t.Helper()
	prevLogger := slog.Default()
	prevWriter, prevFlags := log.Writer(), log.Flags()
	capture := &placementLogCapture{}
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() {
		slog.SetDefault(prevLogger)
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
	})
	return capture
}
