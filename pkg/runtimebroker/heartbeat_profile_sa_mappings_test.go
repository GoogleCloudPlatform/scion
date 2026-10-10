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

package runtimebroker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func saTestReport(name string, gsas ...string) []hubclient.ProfileSAMappingsState {
	st := hubclient.ProfileSAMappingsState{Name: name, ServiceAccountMappings: []hubclient.BrokerProfileSAMapping{}, Complete: true}
	for _, g := range gsas {
		st.ServiceAccountMappings = append(st.ServiceAccountMappings, hubclient.BrokerProfileSAMapping{GSA: g, KSA: "ksa", Namespace: "agents", Source: api.BrokerKSASourceMapped})
	}
	return []hubclient.ProfileSAMappingsState{st}
}

func sendAndCaptureHeartbeat(t *testing.T, hb *HeartbeatService, client *mockRuntimeBrokerService) *hubclient.BrokerHeartbeat {
	t.Helper()
	_ = hb.sendHeartbeat(context.Background())
	client.mu.Lock()
	defer client.mu.Unlock()
	return client.heartbeatCalls[len(client.heartbeatCalls)-1].Heartbeat
}

// Older Hub (empty heartbeat response): today's behaviour is kept. The
// report is sent on the first successful heartbeat, on change, after a
// failed send, and again every saMappingsResendInterval. The hashes are
// sent too (an older Hub ignores them).
func TestHeartbeat_ProfileSAMappingsOlderHub(t *testing.T) {
	client := &mockRuntimeBrokerService{} // heartbeatResp nil: empty body
	hb := NewHeartbeatService(client, "test-host", time.Hour, &mockManager{}, nil, discardLogger())
	current := saTestReport("k8s", "a@example.com")
	hb.profileSAMappings = func() []hubclient.ProfileSAMappingsState { return current }
	send := func() *hubclient.BrokerHeartbeat { return sendAndCaptureHeartbeat(t, hb, client) }

	first := send()
	assert.Equal(t, current, first.ProfileSAMappings, "first heartbeat carries the report")
	require.Len(t, first.ProfileSAMappingsHashes, 1)
	assert.Equal(t, profileSAMappingsHash(current[0]), first.ProfileSAMappingsHashes[0].Hash)
	second := send()
	assert.Nil(t, second.ProfileSAMappings, "unchanged report is not resent")
	assert.Equal(t, first.ProfileSAMappingsHashes, second.ProfileSAMappingsHashes, "the hash is sent on every heartbeat")

	current = saTestReport("k8s")
	client.heartbeatErr = errors.New("hub down")
	assert.Equal(t, current, send().ProfileSAMappings, "a change is sent")
	client.heartbeatErr = nil
	assert.Equal(t, current, send().ProfileSAMappings, "a failed send is retried on the next heartbeat")
	assert.Nil(t, send().ProfileSAMappings)

	// Unchanged report is re-sent once the resend interval has passed.
	hb.mu.Lock()
	hb.sentSAMappingsAt = time.Now().Add(-saMappingsResendInterval - time.Second)
	hb.mu.Unlock()
	assert.Equal(t, current, send().ProfileSAMappings, "unchanged report is re-sent after the interval")
	assert.Nil(t, send().ProfileSAMappings, "and not again until the next interval")

	// Unreadable settings (nil) send nothing and do not reset the state.
	hb.profileSAMappings = func() []hubclient.ProfileSAMappingsState { return nil }
	last := send()
	assert.Nil(t, last.ProfileSAMappings)
	assert.Nil(t, last.ProfileSAMappingsHashes)
}

// A Hub that reads the hashes: an unchanged hash sends no list, not even
// after the resend interval; a Hub request or a change sends it.
func TestHeartbeat_ProfileSAMappingsHashesWithNewHub(t *testing.T) {
	client := &mockRuntimeBrokerService{heartbeatResp: &hubclient.BrokerHeartbeatResponse{ProfileSAMappingsHashes: true}}
	hb := NewHeartbeatService(client, "test-host", time.Hour, &mockManager{}, nil, discardLogger())
	current := saTestReport("k8s", "a@example.com")
	hb.profileSAMappings = func() []hubclient.ProfileSAMappingsState { return current }
	send := func() *hubclient.BrokerHeartbeat { return sendAndCaptureHeartbeat(t, hb, client) }

	assert.Equal(t, current, send().ProfileSAMappings, "first heartbeat carries the report")
	unchanged := send()
	assert.Nil(t, unchanged.ProfileSAMappings, "unchanged hash: no list")
	require.Len(t, unchanged.ProfileSAMappingsHashes, 1, "only the hash")

	hb.mu.Lock()
	hb.sentSAMappingsAt = time.Now().Add(-saMappingsResendInterval - time.Second)
	hb.mu.Unlock()
	assert.Nil(t, send().ProfileSAMappings, "no timed resend to a Hub that reads hashes")

	// The Hub asks for the full report (its stored hash does not match).
	client.mu.Lock()
	client.heartbeatResp = &hubclient.BrokerHeartbeatResponse{ProfileSAMappingsHashes: true, ProfileSAMappingsRequested: true}
	client.mu.Unlock()
	assert.Nil(t, send().ProfileSAMappings, "the request is answered on the next heartbeat")
	client.mu.Lock()
	client.heartbeatResp = &hubclient.BrokerHeartbeatResponse{ProfileSAMappingsHashes: true}
	client.mu.Unlock()
	assert.Equal(t, current, send().ProfileSAMappings, "full report after the Hub asked")
	assert.Nil(t, send().ProfileSAMappings)

	current = saTestReport("k8s", "a@example.com", "b@example.com")
	changed := send()
	assert.Equal(t, current, changed.ProfileSAMappings, "a changed hash sends the list")
	assert.Equal(t, profileSAMappingsHash(current[0]), changed.ProfileSAMappingsHashes[0].Hash)
}

func TestProfileSAMappingsHash(t *testing.T) {
	a := saTestReport("k8s", "a@example.com")[0]
	b := saTestReport("k8s", "a@example.com")[0]
	assert.Equal(t, profileSAMappingsHash(a), profileSAMappingsHash(b), "equal reports hash equally")
	assert.Len(t, profileSAMappingsHash(a), 64)
	b.Complete = false
	assert.NotEqual(t, profileSAMappingsHash(a), profileSAMappingsHash(b), "completeness is part of the hash")
	c := saTestReport("k8s", "a@example.com")[0]
	c.ServiceAccountMappings[0].KSA = "other"
	assert.NotEqual(t, profileSAMappingsHash(a), profileSAMappingsHash(c), "the KSA is part of the hash")
}

func saTestKSA(namespace, name, gsa string) *corev1.ServiceAccount {
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	if gsa != "" {
		sa.Annotations = map[string]string{k8s.WorkloadIdentityGSAAnnotation: gsa}
	}
	return sa
}

func TestServer_HeartbeatProfileSAMappings(t *testing.T) {
	t.Setenv("SCION_K8S_NAMESPACE", "default-ns")
	prev := loadHeartbeatMappingSettings
	t.Cleanup(func() { loadHeartbeatMappingSettings = prev })
	loadHeartbeatMappingSettings = func() (*config.VersionedSettings, error) {
		return &config.VersionedSettings{
			Profiles: map[string]config.V1ProfileConfig{
				"local": {Runtime: "docker"},
				"gke":   {Runtime: "gke-entry", KubernetesServiceAccountMappings: map[string]string{"p@example.com": "p-ksa"}},
				"k8s":   {Runtime: "kubernetes"},
			},
			Runtimes: map[string]config.V1RuntimeConfig{
				"docker":     {Type: "docker"},
				"gke-entry":  {Type: "kubernetes", Namespace: "team-a", KubernetesServiceAccountMappings: map[string]string{"r@example.com": "r-ksa", "p@example.com": "ignored"}},
				"kubernetes": {Type: "kubernetes"},
			},
		}, nil
	}
	client := fake.NewClientset(
		saTestKSA("team-a", "annotated-p", "p@example.com"), // explicit mapping wins
		saTestKSA("team-a", "d-ksa", "d@example.com"),
		saTestKSA("team-a", "amb-1", "amb@example.com"),
		saTestKSA("team-a", "amb-2", "amb@example.com"),
		saTestKSA("default-ns", "k-ksa", "k@example.com"),
	)
	srv := &Server{}
	srv.saDiscoveryCache = newSADiscoveryCache(func(string) (kubernetes.Interface, error) { return client, nil }, discardLogger())

	pending := srv.heartbeatProfileSAMappings()
	require.Equal(t, []hubclient.ProfileSAMappingsState{
		{Name: "gke", ServiceAccountMappings: []hubclient.BrokerProfileSAMapping{
			{GSA: "p@example.com", KSA: "p-ksa", Namespace: "team-a", Source: api.BrokerKSASourceMapped},
			{GSA: "r@example.com", KSA: "r-ksa", Namespace: "team-a", Source: api.BrokerKSASourceMapped},
		}, IncompleteReason: api.BrokerKSADiscoveryPending},
		{Name: "k8s", ServiceAccountMappings: []hubclient.BrokerProfileSAMapping{}, IncompleteReason: api.BrokerKSADiscoveryPending},
	}, pending, "before discovery finishes: explicit entries only, incomplete (pending); Kubernetes profiles only, sorted")

	srv.saDiscoveryCache.wait()
	got := srv.heartbeatProfileSAMappings()
	require.Equal(t, []hubclient.ProfileSAMappingsState{
		{Name: "gke", ServiceAccountMappings: []hubclient.BrokerProfileSAMapping{
			{GSA: "d@example.com", KSA: "d-ksa", Namespace: "team-a", Source: api.BrokerKSASourceDiscovered},
			{GSA: "p@example.com", KSA: "p-ksa", Namespace: "team-a", Source: api.BrokerKSASourceMapped},
			{GSA: "r@example.com", KSA: "r-ksa", Namespace: "team-a", Source: api.BrokerKSASourceMapped},
		}, Complete: true, AmbiguousGSAs: []string{"amb@example.com"}},
		{Name: "k8s", ServiceAccountMappings: []hubclient.BrokerProfileSAMapping{
			{GSA: "k@example.com", KSA: "k-ksa", Namespace: "default-ns", Source: api.BrokerKSASourceDiscovered},
		}, Complete: true},
	}, got, "explicit wins, discovered added, ambiguous listed, namespace per entry (runtime entry, else the runtime default)")

	loadHeartbeatMappingSettings = func() (*config.VersionedSettings, error) { return nil, errors.New("bad") }
	assert.Nil(t, srv.heartbeatProfileSAMappings(), "unreadable settings report nothing")
}

func TestBuildProfileSAReport_DiscoveryFailureIsIncomplete(t *testing.T) {
	vs := &config.VersionedSettings{
		Profiles: map[string]config.V1ProfileConfig{"gke": {Runtime: "k8s", KubernetesServiceAccountMappings: map[string]string{"p@example.com": "p-ksa"}}},
		Runtimes: map[string]config.V1RuntimeConfig{"k8s": {Type: "kubernetes"}},
	}
	for _, code := range []string{api.BrokerKSADiscoveryListFailed, api.BrokerKSADiscoveryUnavailable} {
		got := buildProfileSAReport(vs, "gke", "k8s", "agents", saDiscoveryResult{failure: code, byGSA: map[string][]string{"x@example.com": {"x"}}}, true)
		assert.False(t, got.Complete, code)
		assert.Equal(t, code, got.IncompleteReason)
		assert.Equal(t, []hubclient.BrokerProfileSAMapping{{GSA: "p@example.com", KSA: "p-ksa", Namespace: "agents", Source: api.BrokerKSASourceMapped}}, got.ServiceAccountMappings, "explicit entries are still reported")
	}
}

// Discovery runs in the background on its own cadence, never in lookup,
// and list failures are logged at Warn rate-limited, not per heartbeat.
func TestSADiscoveryCache_BackgroundRefreshAndRateLimitedWarn(t *testing.T) {
	client := fake.NewClientset()
	var lists int
	var mu sync.Mutex
	release := make(chan struct{})
	client.PrependReactor("list", "serviceaccounts", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		<-release
		mu.Lock()
		lists++
		mu.Unlock()
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "serviceaccounts"}, "", errors.New("denied"))
	})
	var logs syncBuffer
	d := newSADiscoveryCache(func(string) (kubernetes.Interface, error) { return client, nil }, slog.New(slog.NewTextHandler(&logs, nil)))
	now := time.Unix(1_000_000, 0)
	d.now = func() time.Time { return now }

	// The list is blocked, yet lookup returns at once (no result yet).
	done := make(chan struct{})
	go func() {
		_, ok := d.lookup("gke", "agents")
		assert.False(t, ok)
		_, ok = d.lookup("gke", "agents") // no second refresh while one runs
		assert.False(t, ok)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("lookup blocked on the API server")
	}
	close(release)
	d.wait()
	res, ok := d.lookup("gke", "agents")
	require.True(t, ok)
	assert.Equal(t, api.BrokerKSADiscoveryListFailed, res.failure)
	assert.Equal(t, 1, strings.Count(logs.String(), "level=WARN"), "first failure warns")

	// Within the interval: cached, no new list.
	for i := 0; i < 5; i++ {
		d.lookup("gke", "agents")
	}
	d.wait()
	mu.Lock()
	assert.Equal(t, 1, lists, "no list within the interval")
	mu.Unlock()

	// After the interval: refreshed, but the same failure does not warn again.
	now = now.Add(saDiscoveryInterval)
	d.lookup("gke", "agents")
	d.wait()
	mu.Lock()
	assert.Equal(t, 2, lists)
	mu.Unlock()
	assert.Equal(t, 1, strings.Count(logs.String(), "level=WARN"), "a repeated failure is not logged again within the warn interval")

	// After the warn interval it is logged again.
	now = now.Add(saDiscoveryWarnInterval)
	d.lookup("gke", "agents")
	d.wait()
	assert.Equal(t, 2, strings.Count(logs.String(), "level=WARN"))
}

func TestSADiscoveryCache_NoClientIsUnavailable(t *testing.T) {
	d := newSADiscoveryCache(func(string) (kubernetes.Interface, error) { return nil, errors.New("no client") }, discardLogger())
	d.lookup("gke", "agents")
	d.wait()
	res, ok := d.lookup("gke", "agents")
	require.True(t, ok)
	assert.Equal(t, api.BrokerKSADiscoveryUnavailable, res.failure)
}
