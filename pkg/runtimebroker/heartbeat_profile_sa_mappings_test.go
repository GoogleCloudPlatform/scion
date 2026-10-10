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
	"net/http"
	"net/http/httptest"
	"os"
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
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
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
	current := saTestReport("k8s", "a@example-project.iam.gserviceaccount.com")
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
	current := saTestReport("k8s", "a@example-project.iam.gserviceaccount.com")
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

	current = saTestReport("k8s", "a@example-project.iam.gserviceaccount.com", "b@example-project.iam.gserviceaccount.com")
	changed := send()
	assert.Equal(t, current, changed.ProfileSAMappings, "a changed hash sends the list")
	assert.Equal(t, profileSAMappingsHash(current[0]), changed.ProfileSAMappingsHashes[0].Hash)
}

func TestProfileSAMappingsHash(t *testing.T) {
	a := saTestReport("k8s", "a@example-project.iam.gserviceaccount.com")[0]
	b := saTestReport("k8s", "a@example-project.iam.gserviceaccount.com")[0]
	assert.Equal(t, profileSAMappingsHash(a), profileSAMappingsHash(b), "equal reports hash equally")
	assert.Len(t, profileSAMappingsHash(a), 64)
	b.Complete = false
	assert.NotEqual(t, profileSAMappingsHash(a), profileSAMappingsHash(b), "completeness is part of the hash")
	c := saTestReport("k8s", "a@example-project.iam.gserviceaccount.com")[0]
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
	// The settings loader is a per-server field: the test never writes
	// state another server's heartbeat loop reads (ptone/scion#4313).
	srv := &Server{}
	srv.loadMappingSettings = func() (*config.VersionedSettings, error) {
		return &config.VersionedSettings{
			Profiles: map[string]config.V1ProfileConfig{
				"local": {Runtime: "docker"},
				"gke":   {Runtime: "gke-entry", KubernetesServiceAccountMappings: map[string]string{"p@example-project.iam.gserviceaccount.com": "p-ksa"}},
				"k8s":   {Runtime: "kubernetes"},
			},
			Runtimes: map[string]config.V1RuntimeConfig{
				"docker":     {Type: "docker"},
				"gke-entry":  {Type: "kubernetes", Namespace: "team-a", KubernetesServiceAccountMappings: map[string]string{"r@example-project.iam.gserviceaccount.com": "r-ksa", "p@example-project.iam.gserviceaccount.com": "ignored"}},
				"kubernetes": {Type: "kubernetes"},
			},
		}, nil
	}
	client := fake.NewClientset(
		saTestKSA("team-a", "annotated-p", "p@example-project.iam.gserviceaccount.com"), // explicit mapping wins
		saTestKSA("team-a", "d-ksa", "d@example-project.iam.gserviceaccount.com"),
		saTestKSA("team-a", "amb-1", "amb@example-project.iam.gserviceaccount.com"),
		saTestKSA("team-a", "amb-2", "amb@example-project.iam.gserviceaccount.com"),
		saTestKSA("default-ns", "k-ksa", "k@example-project.iam.gserviceaccount.com"),
	)
	srv.saDiscoveryCache = newSADiscoveryCache(func(string) (kubernetes.Interface, error) { return client, nil }, discardLogger())

	pending := srv.heartbeatProfileSAMappings()
	require.Equal(t, []hubclient.ProfileSAMappingsState{
		{Name: "gke", ServiceAccountMappings: []hubclient.BrokerProfileSAMapping{
			{GSA: "p@example-project.iam.gserviceaccount.com", KSA: "p-ksa", Namespace: "team-a", Source: api.BrokerKSASourceMapped},
			{GSA: "r@example-project.iam.gserviceaccount.com", KSA: "r-ksa", Namespace: "team-a", Source: api.BrokerKSASourceMapped},
		}, IncompleteReason: api.BrokerKSADiscoveryPending, ReportVersion: api.BrokerSAReportVersion},
		{Name: "k8s", ServiceAccountMappings: []hubclient.BrokerProfileSAMapping{}, IncompleteReason: api.BrokerKSADiscoveryPending, ReportVersion: api.BrokerSAReportVersion},
	}, pending, "before discovery finishes: explicit entries only, incomplete (pending); Kubernetes profiles only, sorted")

	srv.saDiscoveryCache.wait()
	got := srv.heartbeatProfileSAMappings()
	require.Equal(t, []hubclient.ProfileSAMappingsState{
		{Name: "gke", ServiceAccountMappings: []hubclient.BrokerProfileSAMapping{
			{GSA: "d@example-project.iam.gserviceaccount.com", KSA: "d-ksa", Namespace: "team-a", Source: api.BrokerKSASourceDiscovered},
			{GSA: "p@example-project.iam.gserviceaccount.com", KSA: "p-ksa", Namespace: "team-a", Source: api.BrokerKSASourceMapped},
			{GSA: "r@example-project.iam.gserviceaccount.com", KSA: "r-ksa", Namespace: "team-a", Source: api.BrokerKSASourceMapped},
		}, Complete: true, AmbiguousGSAs: []string{"amb@example-project.iam.gserviceaccount.com"}, ReportVersion: api.BrokerSAReportVersion},
		{Name: "k8s", ServiceAccountMappings: []hubclient.BrokerProfileSAMapping{
			{GSA: "k@example-project.iam.gserviceaccount.com", KSA: "k-ksa", Namespace: "default-ns", Source: api.BrokerKSASourceDiscovered},
		}, Complete: true, ReportVersion: api.BrokerSAReportVersion},
	}, got, "explicit wins, discovered added, ambiguous listed, namespace per entry (runtime entry, else the runtime default)")

	// No refresh is running (all finished, and the next is not due), so
	// replacing the loader races with nothing.
	srv.saDiscoveryCache.wait()
	srv.loadMappingSettings = func() (*config.VersionedSettings, error) { return nil, errors.New("bad") }
	assert.Nil(t, srv.heartbeatProfileSAMappings(), "unreadable settings report nothing")
}

func TestBuildProfileSAReport_DiscoveryFailureIsIncomplete(t *testing.T) {
	vs := &config.VersionedSettings{
		Profiles: map[string]config.V1ProfileConfig{"gke": {Runtime: "k8s", KubernetesServiceAccountMappings: map[string]string{"p@example-project.iam.gserviceaccount.com": "p-ksa"}}},
		Runtimes: map[string]config.V1RuntimeConfig{"k8s": {Type: "kubernetes"}},
	}
	for _, code := range []string{api.BrokerKSADiscoveryListFailed, api.BrokerKSADiscoveryUnavailable} {
		got, _ := buildProfileSAReport(vs, "gke", "k8s", "agents", saDiscoveryResult{failure: code, byGSA: map[string][]string{"x@example-project.iam.gserviceaccount.com": {"x"}}}, true)
		assert.False(t, got.Complete, code)
		assert.Equal(t, code, got.IncompleteReason)
		assert.Equal(t, []hubclient.BrokerProfileSAMapping{{GSA: "p@example-project.iam.gserviceaccount.com", KSA: "p-ksa", Namespace: "agents", Source: api.BrokerKSASourceMapped}}, got.ServiceAccountMappings, "explicit entries are still reported")
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

// saDiscoveryClientset picks the client for the cluster a dispatch on the
// profile uses, from the global+overlay settings, and never auto-detects.
func TestServer_SADiscoveryClientset(t *testing.T) {
	t.Setenv("KUBECONFIG", "/tmp/example-kubeconfig")
	settingsFor := func(context string) func() (*config.VersionedSettings, error) {
		return func() (*config.VersionedSettings, error) {
			return &config.VersionedSettings{
				Profiles: map[string]config.V1ProfileConfig{
					"on-default": {Runtime: "entry-a"},
					"on-aux":     {Runtime: "entry-b"},
					"no-live":    {Runtime: "entry-c"},
					"local":      {Runtime: "docker"},
				},
				Runtimes: map[string]config.V1RuntimeConfig{
					"entry-a": {Type: "kubernetes", Context: "ctx-a", Namespace: "agents"},
					"entry-b": {Type: "kubernetes", Context: "ctx-b", Namespace: "agents"},
					"entry-c": {Type: "kubernetes", Context: context, Namespace: "agents"},
					"docker":  {Type: "docker"},
				},
			}, nil
		}
	}
	defClient, auxClient := fake.NewClientset(), fake.NewClientset()
	type build struct{ kubeconfig, context string }
	var builds []build
	built := map[string]kubernetes.Interface{}
	srv := &Server{
		runtime: &scionrt.KubernetesRuntime{Client: &k8s.Client{Clientset: defClient, CurrentContext: "ctx-a"}, DefaultNamespace: "agents"},
		auxiliaryRuntimes: map[string]auxiliaryRuntime{
			"aux": {Runtime: &scionrt.KubernetesRuntime{Client: &k8s.Client{Clientset: auxClient, CurrentContext: "ctx-b"}, DefaultNamespace: "other"}},
		},
		loadMappingSettings: settingsFor("ctx-c"),
	}
	srv.newDiscoveryClient = func(kubeconfig, context string, timeout time.Duration) (kubernetes.Interface, error) {
		assert.Equal(t, assignDiscoveryTimeout, timeout, "the build is bounded by the discovery timeout")
		builds = append(builds, build{kubeconfig, context})
		c := fake.NewClientset()
		built[context] = c
		return c, nil
	}

	c, err := srv.saDiscoveryClientset("on-default")
	require.NoError(t, err)
	assert.Same(t, defClient, c, "the default runtime's client when the profile resolves to it")

	c, err = srv.saDiscoveryClientset("on-aux")
	require.NoError(t, err)
	assert.Same(t, auxClient, c, "a live auxiliary runtime for the same context")
	assert.Empty(t, builds, "no client built while a live runtime serves the profile")

	c, err = srv.saDiscoveryClientset("no-live")
	require.NoError(t, err)
	assert.Same(t, built["ctx-c"], c)
	c2, err := srv.saDiscoveryClientset("no-live")
	require.NoError(t, err)
	assert.Same(t, c, c2, "cached")
	assert.Equal(t, []build{{"/tmp/example-kubeconfig", "ctx-c"}}, builds, "built once, from the kubeconfig and the entry's context")

	// The entry's context changes in the settings (no restart): a new
	// client for the new cluster.
	srv.loadMappingSettings = settingsFor("ctx-d")
	c3, err := srv.saDiscoveryClientset("no-live")
	require.NoError(t, err)
	assert.Same(t, built["ctx-d"], c3)
	assert.NotSame(t, c, c3)
	assert.Len(t, builds, 2)

	_, err = srv.saDiscoveryClientset("local")
	assert.Error(t, err, "a non-Kubernetes profile is unavailable, never auto-detected")
	_, err = srv.saDiscoveryClientset("missing")
	assert.Error(t, err)
	assert.Len(t, builds, 2, "nothing built for them")
}

// A malformed explicit KSA name is not reported (dispatch refuses it), and
// it still wins over discovery for its GSA.
func TestBuildProfileSAReport_MalformedExplicitKSAOmitted(t *testing.T) {
	vs := &config.VersionedSettings{
		Profiles: map[string]config.V1ProfileConfig{"gke": {Runtime: "k8s", KubernetesServiceAccountMappings: map[string]string{
			"bad@example-project.iam.gserviceaccount.com": "Not_A_Valid_Name", "good@example-project.iam.gserviceaccount.com": "good-ksa",
		}}},
		Runtimes: map[string]config.V1RuntimeConfig{"k8s": {Type: "kubernetes"}},
	}
	require.Error(t, config.ValidateKubernetesServiceAccountMappings(map[string]string{"bad@example-project.iam.gserviceaccount.com": "Not_A_Valid_Name"}))
	got, malformed := buildProfileSAReport(vs, "gke", "k8s", "agents", saDiscoveryResult{byGSA: map[string][]string{"bad@example-project.iam.gserviceaccount.com": {"annotated"}}}, true)
	assert.Equal(t, []hubclient.BrokerProfileSAMapping{
		{GSA: "good@example-project.iam.gserviceaccount.com", KSA: "good-ksa", Namespace: "agents", Source: api.BrokerKSASourceMapped},
	}, got.ServiceAccountMappings)
	assert.Equal(t, []string{"bad@example-project.iam.gserviceaccount.com"}, malformed)
	assert.True(t, got.Complete)
}

// The heartbeat report warns about a malformed explicit KSA name, at most
// once per warn interval, not on every heartbeat.
func TestServer_HeartbeatProfileSAMappings_MalformedWarnRateLimited(t *testing.T) {
	var logs syncBuffer
	srv := &Server{loadMappingSettings: func() (*config.VersionedSettings, error) {
		return &config.VersionedSettings{
			Profiles: map[string]config.V1ProfileConfig{"gke": {Runtime: "k8s", KubernetesServiceAccountMappings: map[string]string{"bad@example-project.iam.gserviceaccount.com": "Not_A_Valid_Name"}}},
			Runtimes: map[string]config.V1RuntimeConfig{"k8s": {Type: "kubernetes", Namespace: "agents"}},
		}, nil
	}}
	srv.saDiscoveryCache = newSADiscoveryCache(func(string) (kubernetes.Interface, error) { return fake.NewClientset(), nil }, slog.New(slog.NewTextHandler(&logs, nil)))
	now := time.Unix(1_000_000, 0)
	srv.saDiscoveryCache.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		got := srv.heartbeatProfileSAMappings()
		require.Len(t, got, 1)
		assert.Empty(t, got[0].ServiceAccountMappings)
	}
	srv.saDiscoveryCache.wait()
	assert.Equal(t, 1, strings.Count(logs.String(), "malformed Kubernetes ServiceAccount name"))
	now = now.Add(saDiscoveryWarnInterval)
	srv.heartbeatProfileSAMappings()
	srv.saDiscoveryCache.wait()
	assert.Equal(t, 2, strings.Count(logs.String(), "malformed Kubernetes ServiceAccount name"))
}

// Entries not looked up for two intervals are pruned, and stop ends the
// refreshes: none starts afterwards.
func TestSADiscoveryCache_PruneAndStop(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	d := newSADiscoveryCache(func(string) (kubernetes.Interface, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return fake.NewClientset(), nil
	}, discardLogger())
	now := time.Unix(1_000_000, 0)
	d.now = func() time.Time { return now }
	d.lookup("gke", "old-ns")
	d.wait()
	now = now.Add(2 * saDiscoveryInterval)
	d.lookup("gke", "new-ns")
	d.wait()
	d.mu.Lock()
	_, oldKept := d.entries["gke\x00old-ns"]
	_, newKept := d.entries["gke\x00new-ns"]
	d.mu.Unlock()
	assert.False(t, oldKept, "an entry not looked up for two intervals is pruned")
	assert.True(t, newKept)

	d.stop()
	now = now.Add(saDiscoveryInterval)
	d.lookup("gke", "new-ns")
	d.wait()
	mu.Lock()
	assert.Equal(t, 2, calls, "no refresh after stop")
	mu.Unlock()
}

// The default builder checks the client with k8s.Client.Verify (the
// /version call, where the ADC fallback lives), bounded by the timeout,
// and a client that fails it is not used. Runs against httptest API
// servers; the GCE fallback itself needs a GCE metadata server and is not
// exercised here.
func TestNewKubernetesDiscoveryClient_Verifies(t *testing.T) {
	kubeconfigFor := func(server string) string {
		path := t.TempDir() + "/kubeconfig"
		require.NoError(t, os.WriteFile(path, []byte(`apiVersion: v1
kind: Config
clusters:
- cluster:
    server: `+server+`
  name: c
contexts:
- context:
    cluster: c
    user: u
  name: example-context
current-context: example-context
users:
- name: u
  user:
    token: fake-token
`), 0o600))
		return path
	}
	var mu sync.Mutex
	versionCalls := 0
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			mu.Lock()
			versionCalls++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"major":"1","minor":"30","gitVersion":"v1.30.0"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer ok.Close()
	c, err := newKubernetesDiscoveryClient(kubeconfigFor(ok.URL), "example-context", 3*time.Second)
	require.NoError(t, err)
	require.NotNil(t, c)
	mu.Lock()
	assert.Equal(t, 1, versionCalls, "the client is verified once")
	mu.Unlock()

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer failing.Close()
	_, err = newKubernetesDiscoveryClient(kubeconfigFor(failing.URL), "", 3*time.Second)
	assert.Error(t, err, "a client that fails Verify is not used")

	release := make(chan struct{})
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer hung.Close()
	defer close(release)
	start := time.Now()
	_, err = newKubernetesDiscoveryClient(kubeconfigFor(hung.URL), "", 200*time.Millisecond)
	assert.Error(t, err)
	assert.Less(t, time.Since(start), 5*time.Second, "Verify is bounded by the timeout")
}

// A refresh cut short by stop (server shutdown) records nothing and logs
// nothing.
func TestSADiscoveryCache_StopDuringRefreshIsQuiet(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("list", "serviceaccounts", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, errors.New("context canceled")
	})
	entered, release := make(chan struct{}), make(chan struct{})
	var logs syncBuffer
	d := newSADiscoveryCache(func(string) (kubernetes.Interface, error) {
		close(entered)
		<-release
		return client, nil
	}, slog.New(slog.NewTextHandler(&logs, nil)))
	_, ok := d.lookup("gke", "agents")
	require.False(t, ok)
	<-entered
	d.stop()
	close(release)
	d.wait()
	_, ok = d.lookup("gke", "agents")
	assert.False(t, ok, "no result recorded for a refresh stopped mid-way")
	d.mu.Lock()
	assert.False(t, d.entries["gke\x00agents"].running)
	d.mu.Unlock()
	assert.NotContains(t, logs.String(), "level=WARN", "no warning at shutdown")
}

// A broker with ForceRuntime set ignores the profile at dispatch, so it
// reports every Kubernetes profile incomplete (force_runtime), runs no
// discovery for it, and the hash differs from the complete report.
func TestServer_HeartbeatProfileSAMappings_ForceRuntimeIncomplete(t *testing.T) {
	settings := func() (*config.VersionedSettings, error) {
		return &config.VersionedSettings{
			Profiles: map[string]config.V1ProfileConfig{"gke": {Runtime: "k8s", KubernetesServiceAccountMappings: map[string]string{"p@example-project.iam.gserviceaccount.com": "p-ksa"}}},
			Runtimes: map[string]config.V1RuntimeConfig{"k8s": {Type: "kubernetes", Namespace: "agents"}},
		}, nil
	}
	lookups := 0
	clientFor := func(string) (kubernetes.Interface, error) { lookups++; return fake.NewClientset(), nil }

	normal := &Server{loadMappingSettings: settings}
	normal.saDiscoveryCache = newSADiscoveryCache(clientFor, discardLogger())
	normal.heartbeatProfileSAMappings()
	normal.saDiscoveryCache.wait()
	complete := normal.heartbeatProfileSAMappings()
	require.Len(t, complete, 1)
	require.True(t, complete[0].Complete)

	forced := &Server{loadMappingSettings: settings, config: ServerConfig{ForceRuntime: "kubernetes"}}
	forced.saDiscoveryCache = newSADiscoveryCache(func(string) (kubernetes.Interface, error) {
		t.Error("no discovery for a profile under ForceRuntime")
		return fake.NewClientset(), nil
	}, discardLogger())
	got := forced.heartbeatProfileSAMappings()
	forced.saDiscoveryCache.wait()
	require.Equal(t, []hubclient.ProfileSAMappingsState{
		{Name: "gke", ServiceAccountMappings: []hubclient.BrokerProfileSAMapping{
			{GSA: "p@example-project.iam.gserviceaccount.com", KSA: "p-ksa", Namespace: "agents", Source: api.BrokerKSASourceMapped},
		}, IncompleteReason: api.BrokerSAReportForceRuntime, ReportVersion: api.BrokerSAReportVersion},
	}, got)
	assert.NotEqual(t, profileSAMappingsHash(complete[0]), profileSAMappingsHash(got[0]),
		"the changed hash makes the Hub replace a stored complete report")
	assert.Equal(t, 1, lookups)
}
