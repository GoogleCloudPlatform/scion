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
	"errors"
	"testing"
)

// liveContext returns a current-context reader that counts its calls.
func liveContext(ctx string, err error, calls *int) func() (string, error) {
	return func() (string, error) {
		*calls++
		return ctx, err
	}
}

// noLiveRead returns a current-context reader that fails the test if it is
// called.
func noLiveRead(t *testing.T) func() (string, error) {
	t.Helper()
	return func() (string, error) {
		t.Fatal("live current-context read must not be invoked")
		return "", nil
	}
}

var presentRecord = RuntimeBuildRecord{Present: true, EntryName: "k8s", KubeconfigContext: "ctx-a"}

func requireMatch(t *testing.T, got MatchResult, cause string, want MatchResult) {
	t.Helper()
	if got != want {
		t.Fatalf("MatchKubernetesEntry = %v (cause %q), want %v", got, cause, want)
	}
	if (want == Undetermined) != (cause != "") {
		t.Fatalf("cause %q for result %v: a cause is set exactly when Undetermined", cause, got)
	}
}

// U1: Present == false or hasClient == false is Undetermined.
func TestMatchKubernetesEntry_U1_NoRecordOrClient(t *testing.T) {
	entry := V1RuntimeConfig{Type: "kubernetes", Namespace: "ns"}
	for _, conn := range []KubeConnection{KubeConnKubeconfig, KubeConnInCluster, KubeConnUnknown} {
		got, cause := MatchKubernetesEntry(entry, "ctx-a", "ns", true, conn, RuntimeBuildRecord{}, "ns", noLiveRead(t))
		requireMatch(t, got, cause, Undetermined)
		if cause != matchCauseNoRecord {
			t.Fatalf("cause = %q, want %q", cause, matchCauseNoRecord)
		}
		got, cause = MatchKubernetesEntry(entry, "ctx-a", "ns", false, conn, presentRecord, "ns", noLiveRead(t))
		requireMatch(t, got, cause, Undetermined)
		if cause != matchCauseNoRecord {
			t.Fatalf("cause = %q, want %q", cause, matchCauseNoRecord)
		}
	}
}

// U2: kubeconfig connection, empty-context entry, live current-context
// unreadable is Undetermined.
func TestMatchKubernetesEntry_U2_LiveContextUnreadable(t *testing.T) {
	entry := V1RuntimeConfig{Type: "kubernetes", Namespace: "ns"}
	calls := 0
	got, cause := MatchKubernetesEntry(entry, "ctx-a", "ns", true, KubeConnKubeconfig, presentRecord, "ns",
		liveContext("", errors.New("unreadable"), &calls))
	requireMatch(t, got, cause, Undetermined)
	if cause != matchCauseUnreadable || calls != 1 {
		t.Fatalf("cause = %q calls = %d, want %q and one call", cause, calls, matchCauseUnreadable)
	}
	// A nil reader counts as unreadable.
	got, cause = MatchKubernetesEntry(entry, "ctx-a", "ns", true, KubeConnKubeconfig, presentRecord, "ns", nil)
	requireMatch(t, got, cause, Undetermined)
	if cause != matchCauseUnreadable {
		t.Fatalf("nil reader: cause = %q, want %q", cause, matchCauseUnreadable)
	}
}

// U3: kubeconfig connection, empty-context entry, exactly one of the
// recorded (R) and live (L) current contexts equals C is Undetermined.
func TestMatchKubernetesEntry_U3_ExactlyOneEqualsC(t *testing.T) {
	entry := V1RuntimeConfig{Type: "kubernetes", Namespace: "ns"}
	for _, tc := range []struct{ name, recorded, live string }{
		{"recorded equals C", "ctx-a", "ctx-b"},
		{"live equals C", "ctx-b", "ctx-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			rec := RuntimeBuildRecord{Present: true, KubeconfigContext: tc.recorded}
			got, cause := MatchKubernetesEntry(entry, "ctx-a", "ns", true, KubeConnKubeconfig, rec, "ns", liveContext(tc.live, nil, &calls))
			requireMatch(t, got, cause, Undetermined)
			if cause != matchCauseContextChanged {
				t.Fatalf("cause = %q, want %q", cause, matchCauseContextChanged)
			}
		})
	}
}

// U3b: kubeconfig connection, empty-context entry, neither R nor L equals
// C is NoMatch.
func TestMatchKubernetesEntry_U3b_NeitherEqualsC(t *testing.T) {
	entry := V1RuntimeConfig{Type: "kubernetes", Namespace: "ns"}
	calls := 0
	rec := RuntimeBuildRecord{Present: true, KubeconfigContext: "ctx-b"}
	got, cause := MatchKubernetesEntry(entry, "ctx-a", "ns", true, KubeConnKubeconfig, rec, "ns", liveContext("ctx-c", nil, &calls))
	requireMatch(t, got, cause, NoMatch)
	if calls != 1 {
		t.Fatalf("live reads = %d, want 1", calls)
	}
}

// U3c: kubeconfig connection, empty-context entry, both R and L equal C is
// Match, and the namespace is still checked.
func TestMatchKubernetesEntry_U3c_BothEqualC(t *testing.T) {
	calls := 0
	rec := RuntimeBuildRecord{Present: true, KubeconfigContext: "ctx-a"}
	got, cause := MatchKubernetesEntry(V1RuntimeConfig{Type: "kubernetes", Namespace: "ns"}, "ctx-a", "ns", true,
		KubeConnKubeconfig, rec, "default", liveContext("ctx-a", nil, &calls))
	requireMatch(t, got, cause, Match)
	got, cause = MatchKubernetesEntry(V1RuntimeConfig{Type: "kubernetes", Namespace: "other"}, "ctx-a", "ns", true,
		KubeConnKubeconfig, rec, "default", liveContext("ctx-a", nil, &calls))
	requireMatch(t, got, cause, NoMatch)
	if calls != 2 {
		t.Fatalf("live reads = %d, want 2", calls)
	}
}

// U6: kubeconfig connection, explicit-context entry is compared with the
// runtime's C; the live-read callback is not invoked.
func TestMatchKubernetesEntry_U6_ExplicitContextComparedWithC(t *testing.T) {
	rec := RuntimeBuildRecord{Present: true, KubeconfigContext: "something-else"}
	got, cause := MatchKubernetesEntry(V1RuntimeConfig{Type: "kubernetes", Context: "ctx-a", Namespace: "ns"}, "ctx-a", "ns", true,
		KubeConnKubeconfig, rec, "default", noLiveRead(t))
	requireMatch(t, got, cause, Match)
	got, cause = MatchKubernetesEntry(V1RuntimeConfig{Type: "kubernetes", Context: "ctx-b", Namespace: "ns"}, "ctx-a", "ns", true,
		KubeConnKubeconfig, rec, "default", noLiveRead(t))
	requireMatch(t, got, cause, NoMatch)
	got, cause = MatchKubernetesEntry(V1RuntimeConfig{Type: "kubernetes", Context: "ctx-a", Namespace: "other"}, "ctx-a", "ns", true,
		KubeConnKubeconfig, rec, "default", noLiveRead(t))
	requireMatch(t, got, cause, NoMatch)
}

// U9: in-cluster connection with the sentinel C == "in-cluster" and an
// empty-context entry is Match; the live-read callback is not invoked.
func TestMatchKubernetesEntry_U9_InClusterSentinel(t *testing.T) {
	rec := RuntimeBuildRecord{Present: true}
	got, cause := MatchKubernetesEntry(V1RuntimeConfig{Type: "kubernetes", Namespace: "ns"}, "in-cluster", "ns", true,
		KubeConnInCluster, rec, "default", noLiveRead(t))
	requireMatch(t, got, cause, Match)
	got, cause = MatchKubernetesEntry(V1RuntimeConfig{Type: "kubernetes", Namespace: "other"}, "in-cluster", "ns", true,
		KubeConnInCluster, rec, "default", noLiveRead(t))
	requireMatch(t, got, cause, NoMatch)
}

// U9b: in-cluster connection with C == "" (deferred-loader fallback) and
// an empty-context entry is Match; the live-read callback is not invoked.
func TestMatchKubernetesEntry_U9b_InClusterDeferredFallback(t *testing.T) {
	got, cause := MatchKubernetesEntry(V1RuntimeConfig{Type: "kubernetes", Namespace: "ns"}, "", "ns", true,
		KubeConnInCluster, RuntimeBuildRecord{Present: true}, "default", noLiveRead(t))
	requireMatch(t, got, cause, Match)
}

// U10: an empty-namespace entry uses the default namespace (on the agent
// runtime host, defaultKubernetesNamespace(), here POD_NAMESPACE's value) and
// matches a runtime in that namespace. pkg/runtime has the variant that
// reads the namespace from the environment.
func TestMatchKubernetesEntry_U10_EmptyNamespaceUsesDefault(t *testing.T) {
	entry := V1RuntimeConfig{Type: "kubernetes"}
	got, cause := MatchKubernetesEntry(entry, "", "pod-ns", true, KubeConnInCluster, RuntimeBuildRecord{Present: true}, "pod-ns", noLiveRead(t))
	requireMatch(t, got, cause, Match)
	got, cause = MatchKubernetesEntry(entry, "", "pod-ns", true, KubeConnInCluster, RuntimeBuildRecord{Present: true}, "default", noLiveRead(t))
	requireMatch(t, got, cause, NoMatch)
	calls := 0
	got, cause = MatchKubernetesEntry(V1RuntimeConfig{Type: "kubernetes", Context: "ctx-a"}, "ctx-a", "pod-ns", true,
		KubeConnKubeconfig, presentRecord, "pod-ns", liveContext("ctx-a", nil, &calls))
	requireMatch(t, got, cause, Match)
}

// U11: in-cluster connection (sentinel C), explicit-context entry is
// NoMatch; the live-read callback is not invoked.
func TestMatchKubernetesEntry_U11_InClusterSentinelExplicitContext(t *testing.T) {
	got, cause := MatchKubernetesEntry(V1RuntimeConfig{Type: "kubernetes", Context: "in-cluster", Namespace: "ns"}, "in-cluster", "ns", true,
		KubeConnInCluster, RuntimeBuildRecord{Present: true}, "default", noLiveRead(t))
	requireMatch(t, got, cause, NoMatch)
	got, cause = MatchKubernetesEntry(V1RuntimeConfig{Type: "kubernetes", Context: "ctx-a", Namespace: "ns"}, "in-cluster", "ns", true,
		KubeConnInCluster, RuntimeBuildRecord{Present: true}, "default", noLiveRead(t))
	requireMatch(t, got, cause, NoMatch)
}

// U11b: in-cluster connection (C == ""), explicit-context entry is
// NoMatch; the live-read callback is not invoked.
func TestMatchKubernetesEntry_U11b_InClusterDeferredExplicitContext(t *testing.T) {
	got, cause := MatchKubernetesEntry(V1RuntimeConfig{Type: "kubernetes", Context: "ctx-a", Namespace: "ns"}, "", "ns", true,
		KubeConnInCluster, RuntimeBuildRecord{Present: true}, "default", noLiveRead(t))
	requireMatch(t, got, cause, NoMatch)
}

// U12: unknown connection (C == "" with a non-empty raw kubeconfig at
// build) is Undetermined.
func TestMatchKubernetesEntry_U12_UnknownConnection(t *testing.T) {
	rec := RuntimeBuildRecord{Present: true, KubeconfigContext: "ctx-a"}
	for _, entry := range []V1RuntimeConfig{
		{Type: "kubernetes", Namespace: "ns"},
		{Type: "kubernetes", Context: "ctx-a", Namespace: "ns"},
	} {
		got, cause := MatchKubernetesEntry(entry, "", "ns", true, KubeConnUnknown, rec, "default", noLiveRead(t))
		requireMatch(t, got, cause, Undetermined)
		if cause != matchCauseUnknownConn {
			t.Fatalf("cause = %q, want %q", cause, matchCauseUnknownConn)
		}
	}
}

// U13: in-cluster connection, and a kubeconfig with a current context
// appears after build: still Match; the live-read callback is not invoked.
func TestMatchKubernetesEntry_U13_InClusterIgnoresLaterKubeconfig(t *testing.T) {
	got, cause := MatchKubernetesEntry(V1RuntimeConfig{Type: "kubernetes", Namespace: "ns"}, "", "ns", true,
		KubeConnInCluster, RuntimeBuildRecord{Present: true}, "default",
		func() (string, error) {
			t.Fatal("live current-context read must not be invoked")
			return "ctx-appeared", nil
		})
	requireMatch(t, got, cause, Match)
}

// U14: kubeconfig connection, explicit-context runtime (C is the requested
// name), empty-context entry, R is the raw kubeconfig current context and
// differs from C, L == R: NoMatch.
func TestMatchKubernetesEntry_U14_ExplicitContextRuntimeEmptyContextEntry(t *testing.T) {
	calls := 0
	rec := RuntimeBuildRecord{Present: true, KubeconfigContext: "raw-current"}
	got, cause := MatchKubernetesEntry(V1RuntimeConfig{Type: "kubernetes", Namespace: "ns"}, "requested", "ns", true,
		KubeConnKubeconfig, rec, "default", liveContext("raw-current", nil, &calls))
	requireMatch(t, got, cause, NoMatch)
	if calls != 1 {
		t.Fatalf("live reads = %d, want 1", calls)
	}
}
