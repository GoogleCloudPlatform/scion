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
	"fmt"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// This file builds the per-profile GCP service account report a broker
// sends on its heartbeat (ptone/scion#3329 phase 4): for each Kubernetes
// profile, the GSAs it can serve in GCP identity mode "assign" and the
// Kubernetes ServiceAccount (KSA) each would run as, from the explicit
// kubernetes_service_account_mappings and from annotation discovery.
//
// Discovery never runs on the heartbeat path. Each (profile, namespace)
// has a cached result that a background lookup refreshes every
// saDiscoveryInterval; the heartbeat reads whatever is cached. Until the
// first lookup finishes, the profile is reported incomplete (pending).

const (
	// saDiscoveryInterval is how often the cached discovery result of a
	// (profile, namespace) is refreshed.
	saDiscoveryInterval = 5 * time.Minute
	// saDiscoveryWarnInterval limits list failure warnings to one per
	// (profile, namespace) in this interval, unless the failure changes.
	saDiscoveryWarnInterval = 30 * time.Minute
)

// heartbeatProfileSAMappings returns, sorted by profile name, the report of
// each Kubernetes profile (by resolved runtime type) in the broker's global
// settings, for the heartbeat's ProfileSAMappings field. Nil when the
// settings cannot be read, so the hub keeps what it has; an empty result
// when there are no Kubernetes profiles.
func (s *Server) heartbeatProfileSAMappings() []hubclient.ProfileSAMappingsState {
	vs, err := s.loadHeartbeatMappingSettings()
	if err != nil || vs == nil {
		return nil
	}
	names := make([]string, 0, len(vs.Profiles))
	for name := range vs.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	disc := s.saDiscovery()
	out := []hubclient.ProfileSAMappingsState{}
	for _, name := range names {
		if _, isKubernetes, _ := vs.ProfileKubernetesSAMappings(name); !isKubernetes {
			continue
		}
		entry := vs.Profiles[name].Runtime
		// The same namespace a dispatch resolves for the Workload Identity
		// principal (resolveKubernetesAssignIdentity).
		namespace := resolveAssignNamespace(vs, entry)
		result, ok := disc.lookup(name, namespace)
		state, malformed := buildProfileSAReport(vs, name, entry, namespace, result, ok)
		for _, gsa := range malformed {
			disc.warnRateLimited("malformed\x00"+name+"\x00"+gsa,
				"kubernetes_service_account_mappings entry has a malformed Kubernetes ServiceAccount name; it is left out of the heartbeat service account report, and dispatch refuses it",
				"profile", name, "service_account", gsa)
		}
		out = append(out, state)
	}
	return out
}

// buildProfileSAReport builds one profile's report from its explicit
// mappings and the cached discovery result (ok false when none exists
// yet). An explicit mapping wins over discovery, as at dispatch; a GSA
// that only discovery finds, on more than one KSA, is ambiguous.
//
// malformed lists, sorted, the explicitly mapped GSAs whose KSA name fails
// validation: dispatch refuses them, so they are left out of the report.
func buildProfileSAReport(vs *config.VersionedSettings, profile, entry, namespace string, result saDiscoveryResult, ok bool) (state hubclient.ProfileSAMappingsState, malformed []string) {
	state = hubclient.ProfileSAMappingsState{Name: profile, ServiceAccountMappings: []hubclient.BrokerProfileSAMapping{}}
	explicit := map[string]bool{}
	for _, gsa := range vs.KubernetesServiceAccountMappingGSAs(profile, entry) {
		ksa, _ := vs.ResolveKubernetesServiceAccountMappingForSelection(profile, entry, gsa)
		explicit[gsa] = true
		// Dispatch refuses a malformed entry (resolveKubernetesAssignIdentity
		// validates it at use), so it is not reported as usable. It still
		// wins over discovery, as at dispatch.
		if config.ValidateKubernetesServiceAccountMappings(map[string]string{gsa: ksa}) != nil {
			malformed = append(malformed, gsa)
			continue
		}
		state.ServiceAccountMappings = append(state.ServiceAccountMappings, hubclient.BrokerProfileSAMapping{
			GSA: gsa, KSA: ksa, Namespace: namespace, Source: api.BrokerKSASourceMapped,
		})
	}
	switch {
	case !ok:
		state.IncompleteReason = api.BrokerKSADiscoveryPending
	case result.failure != "":
		state.IncompleteReason = result.failure
	default:
		state.Complete = true
		for gsa, ksas := range result.byGSA {
			if explicit[gsa] {
				continue
			}
			if len(ksas) > 1 {
				state.AmbiguousGSAs = append(state.AmbiguousGSAs, gsa)
				continue
			}
			state.ServiceAccountMappings = append(state.ServiceAccountMappings, hubclient.BrokerProfileSAMapping{
				GSA: gsa, KSA: ksas[0], Namespace: namespace, Source: api.BrokerKSASourceDiscovered,
			})
		}
	}
	sort.Slice(state.ServiceAccountMappings, func(i, j int) bool {
		return state.ServiceAccountMappings[i].GSA < state.ServiceAccountMappings[j].GSA
	})
	sort.Strings(state.AmbiguousGSAs)
	return state, malformed
}

// saDiscoveryResult is one (profile, namespace) discovery lookup: the
// annotated KSAs by GSA, or a fixed failure code
// (api.BrokerKSADiscoveryUnavailable or api.BrokerKSADiscoveryListFailed).
type saDiscoveryResult struct {
	byGSA   map[string][]string
	failure string
}

type saDiscoveryEntry struct {
	result      saDiscoveryResult
	has         bool
	startedAt   time.Time
	lastLookup  time.Time
	running     bool
	lastWarn    time.Time
	lastFailure string
}

// saDiscoveryCache holds the cached discovery result of each (profile,
// namespace) and refreshes it in the background.
type saDiscoveryCache struct {
	interval  time.Duration
	timeout   time.Duration
	now       func() time.Time
	clientFor func(profile string) (kubernetes.Interface, error)
	log       *slog.Logger

	// ctx bounds every refresh and is cancelled by stop (server shutdown).
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	entries  map[string]*saDiscoveryEntry
	warnedAt map[string]time.Time // warnRateLimited keys (guarded by mu)
	wg       sync.WaitGroup
}

// warnRateLimited logs msg at Warn at most once per
// saDiscoveryWarnInterval for key.
func (d *saDiscoveryCache) warnRateLimited(key, msg string, attrs ...any) {
	d.mu.Lock()
	now := d.now()
	last, seen := d.warnedAt[key]
	if seen && now.Sub(last) < saDiscoveryWarnInterval {
		d.mu.Unlock()
		return
	}
	if d.warnedAt == nil {
		d.warnedAt = map[string]time.Time{}
	}
	d.warnedAt[key] = now
	d.mu.Unlock()
	d.log.Warn(msg, attrs...)
}

func newSADiscoveryCache(clientFor func(profile string) (kubernetes.Interface, error), log *slog.Logger) *saDiscoveryCache {
	ctx, cancel := context.WithCancel(context.Background())
	return &saDiscoveryCache{
		interval:  saDiscoveryInterval,
		timeout:   assignDiscoveryTimeout,
		now:       time.Now,
		clientFor: clientFor,
		log:       log,
		ctx:       ctx,
		cancel:    cancel,
		entries:   map[string]*saDiscoveryEntry{},
	}
}

// stop cancels running refreshes and starts no new ones.
func (d *saDiscoveryCache) stop() { d.cancel() }

// lookup returns the cached result for (profile, namespace) and whether one
// exists, and starts a background refresh when the result is missing or
// older than the interval and no refresh is running. It never blocks on the
// API server.
func (d *saDiscoveryCache) lookup(profile, namespace string) (saDiscoveryResult, bool) {
	key := profile + "\x00" + namespace
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	// Drop entries no report has asked for in two intervals (a removed
	// profile, or a namespace the profile no longer resolves to).
	for k, old := range d.entries {
		if k != key && !old.running && now.Sub(old.lastLookup) >= 2*d.interval {
			delete(d.entries, k)
		}
	}
	e := d.entries[key]
	if e == nil {
		e = &saDiscoveryEntry{}
		d.entries[key] = e
	}
	e.lastLookup = now
	if d.ctx.Err() == nil && !e.running && (e.startedAt.IsZero() || now.Sub(e.startedAt) >= d.interval) {
		e.running = true
		e.startedAt = d.now()
		d.wg.Add(1)
		go d.refresh(key, profile, namespace)
	}
	return e.result, e.has
}

// wait blocks until every running refresh has finished. Tests use it.
func (d *saDiscoveryCache) wait() { d.wg.Wait() }

func (d *saDiscoveryCache) refresh(key, profile, namespace string) {
	defer d.wg.Done()
	result, cause := d.discover(profile, namespace)
	d.mu.Lock()
	defer d.mu.Unlock()
	e := d.entries[key]
	e.result, e.has, e.running = result, true, false
	if result.failure == "" {
		e.lastFailure = ""
		return
	}
	// Rate-limited: one warning per interval, or when the failure changes.
	if result.failure != e.lastFailure || d.now().Sub(e.lastWarn) >= saDiscoveryWarnInterval {
		e.lastWarn = d.now()
		d.log.Warn("Kubernetes ServiceAccount annotation discovery for the heartbeat service account report failed; the profile is reported incomplete",
			"profile", profile, "namespace", namespace, "discovery", result.failure, "error", cause)
	}
	e.lastFailure = result.failure
}

func (d *saDiscoveryCache) discover(profile, namespace string) (saDiscoveryResult, error) {
	client, err := d.clientFor(profile)
	if err != nil {
		return saDiscoveryResult{failure: api.BrokerKSADiscoveryUnavailable}, err
	}
	ctx, cancel := context.WithTimeout(d.ctx, d.timeout)
	defer cancel()
	byGSA, err := k8s.ServiceAccountsByGSA(ctx, client, namespace)
	if err != nil {
		return saDiscoveryResult{failure: api.BrokerKSADiscoveryListFailed}, err
	}
	return saDiscoveryResult{byGSA: byGSA}, nil
}

// saDiscovery returns the server's discovery cache, creating it on first
// use.
func (s *Server) saDiscovery() *saDiscoveryCache {
	s.saDiscoveryOnce.Do(func() {
		if s.saDiscoveryCache == nil {
			s.saDiscoveryCache = newSADiscoveryCache(s.saDiscoveryClientset, logging.Subsystem("broker.sa-report"))
		}
	})
	return s.saDiscoveryCache
}

// saDiscoveryClientset returns a Kubernetes client for a profile's
// discovery lookup, for the cluster a dispatch on that profile uses. The
// runtime entry is resolved from the same settings as the mappings and the
// namespace (global plus the DB overlay). A profile whose runtime is not
// Kubernetes is unavailable; nothing is auto-detected. The client is, in
// order: the default runtime's when the profile resolves to it, a live
// auxiliary Kubernetes runtime's for the same context, else one built
// from the kubeconfig and the entry's context, the way runtime.GetRuntime
// builds it. Built clients are cached by (runtime type, kubeconfig,
// context), so a
// context change in the settings selects another client. A built client
// is verified against the cluster within the discovery timeout. It runs
// only in the background refresh.
func (s *Server) saDiscoveryClientset(profile string) (kubernetes.Interface, error) {
	vs, err := s.loadHeartbeatMappingSettings()
	if err != nil {
		return nil, err
	}
	if vs == nil {
		return nil, errors.New("no broker settings")
	}
	rtConfig, rtType, err := vs.ResolveRuntime(profile)
	if err != nil {
		return nil, err
	}
	switch rtType {
	// The same names ProfileKubernetesSAMappings treats as Kubernetes.
	case "kubernetes", "k8s", "remote":
	default:
		return nil, fmt.Errorf("profile %q resolves to runtime type %q, not Kubernetes", profile, rtType)
	}
	s.mu.RLock()
	def := s.runtime
	s.mu.RUnlock()
	if def != nil && runtimeMatchesProfile(def, rtType, rtConfig) {
		if c := kubernetesClientset(def); c != nil {
			return c, nil
		}
	}
	if rtConfig.Context != "" {
		s.auxiliaryRuntimesMu.RLock()
		for _, aux := range s.auxiliaryRuntimes {
			if k, ok := aux.Runtime.(*scionrt.KubernetesRuntime); ok && k.Client != nil && k.Client.CurrentContext == rtConfig.Context {
				if c := kubernetesClientset(k); c != nil {
					s.auxiliaryRuntimesMu.RUnlock()
					return c, nil
				}
			}
		}
		s.auxiliaryRuntimesMu.RUnlock()
	}
	kubeconfig := os.Getenv("KUBECONFIG")
	key := rtType + "\x00" + kubeconfig + "\x00" + rtConfig.Context
	s.saDiscoveryClientsMu.Lock()
	c, ok := s.saDiscoveryClients[key]
	s.saDiscoveryClientsMu.Unlock()
	if ok {
		return c, nil
	}
	// Built (and verified, bounded by the discovery timeout) outside the
	// lock; a concurrent build for the same key is accepted and the last
	// one stored wins. A failed build is not cached, so the next refresh
	// retries it.
	build := s.newDiscoveryClient
	if build == nil {
		build = newKubernetesDiscoveryClient
	}
	c, err = build(kubeconfig, rtConfig.Context, s.saDiscovery().timeout)
	if err != nil {
		return nil, err
	}
	s.saDiscoveryClientsMu.Lock()
	if s.saDiscoveryClients == nil {
		s.saDiscoveryClients = map[string]kubernetes.Interface{}
	}
	s.saDiscoveryClients[key] = c
	s.saDiscoveryClientsMu.Unlock()
	return c, nil
}

// newKubernetesDiscoveryClient builds a Kubernetes client for kubeconfig
// and context (the current context when empty, or the in-cluster config),
// as runtime.GetRuntime does, and verifies it reaches the cluster. Every
// request of the client, the verification included, is bounded by
// timeout, so a hung API server cannot hold a refresh open.
func newKubernetesDiscoveryClient(kubeconfig, context string, timeout time.Duration) (kubernetes.Interface, error) {
	c, err := k8s.NewClientWithContext(kubeconfig, context)
	if err != nil {
		return nil, err
	}
	if c.Config == nil {
		return nil, errors.New("the Kubernetes client has no REST config")
	}
	cfg := rest.CopyConfig(c.Config)
	cfg.Timeout = timeout
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	if _, err := cs.Discovery().ServerVersion(); err != nil {
		return nil, fmt.Errorf("failed to connect to Kubernetes cluster: %w", err)
	}
	return cs, nil
}

// kubernetesClientset returns rt's Kubernetes client, or nil when rt is not
// a Kubernetes runtime with one.
func kubernetesClientset(rt scionrt.Runtime) kubernetes.Interface {
	k, ok := rt.(*scionrt.KubernetesRuntime)
	if !ok || k == nil || k.Client == nil || k.Client.Clientset == nil {
		return nil
	}
	return k.Client.Clientset
}
