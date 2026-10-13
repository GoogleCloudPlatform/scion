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
	"sort"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// HostNFSMounter is the single NFS mount owner of a host that runs several
// Runtime Broker instances (ptone/scion#3274). It replaces each
// instance mounting the host's shares itself:
//
//   - One reconciler and one loop: the instances that bind host paths into
//     their agents (every runtime except Kubernetes and Cloud Run, see
//     NFSWarnOnlyRuntime) register their mount requirements while they are
//     built, and the mounter reconciles the UNION of those requirements.
//     Those instances share the mounter's reconciler: their health shows
//     its status, and a dispatch's EnsureShareMounted goes through it, so
//     every mount on the host is made by this one owner (serialized by the
//     reconciler).
//   - Kubernetes and Cloud Run instances never register: the platform
//     mounts the export into their agents, and they keep their own
//     verify-only check, which never mounts.
//   - Incompatible requirements (one share ID from two different sources,
//     or different mount roots or mount options, which would put two
//     sources on one mount path or mount one path two ways) are refused:
//     the registering instance does not serve.
//   - Nothing is unmounted at shutdown, so stopping one instance (or the
//     host) never removes a mount another instance or a running agent
//     uses; the only unmount is the reconciler's remount of a target found
//     mounted from a source no registered requirement asks for.
//   - Ordering: the host starts the loop before any instance's services
//     and stops it after every instance has shut down (brokerhost.Service).
//   - An instance whose setup already failed when it is built does not
//     register. An instance that stops serving later (its services fail
//     to start, after the loop started) keeps its requirement in the
//     union until the process exits: the union cannot change once the
//     loop runs, and its mounts would stay anyway.
type HostNFSMounter struct {
	checker  MountChecker
	log      *slog.Logger
	interval time.Duration

	mu         sync.Mutex
	cfg        *config.V1NFSConfig // the union; nil until a requirement registers
	sources    map[string]string   // share ID -> instance key that first required it
	reconciler *NFSMountReconciler
	started    bool

	firstPass chan struct{}
	stopped   chan struct{}
	cancel    context.CancelFunc
}

// NewHostNFSMounter returns a mounter with no requirements. checker nil
// uses the exec mount layer.
func NewHostNFSMounter(checker MountChecker, log *slog.Logger) *HostNFSMounter {
	if log == nil {
		log = slog.Default()
	}
	if checker == nil {
		checker = NewExecMountChecker(log)
	}
	return &HostNFSMounter{checker: checker, log: log, sources: map[string]string{},
		firstPass: make(chan struct{}), stopped: make(chan struct{})}
}

// ErrNFSRequirementIncompatible is a mount requirement that conflicts with
// one an earlier instance registered.
var ErrNFSRequirementIncompatible = errors.New("NFS mount requirement is incompatible with another instance's on this host")

// Register adds an instance's mount requirement to the union and returns
// the shared reconciler the instance uses. A runtime whose platform mounts
// the export (NFSWarnOnlyRuntime) and an empty requirement register
// nothing (nil, nil). Registering after Start, or a requirement that
// conflicts with the union, is an error.
func (m *HostNFSMounter) Register(instanceKey, runtimeName string, req *config.V1NFSConfig) (*NFSMountReconciler, error) {
	if req == nil || len(req.Shares) == 0 || NFSWarnOnlyRuntime(runtimeName) {
		return nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started {
		return nil, fmt.Errorf("NFS mount requirement of instance %q registered after the host mounter started", instanceKey)
	}
	if m.cfg == nil {
		m.cfg = &config.V1NFSConfig{MountRoot: req.MountRoot, MountOptions: req.MountOptions, AutoMount: req.AutoMount}
	} else {
		if req.MountRoot != m.cfg.MountRoot {
			return nil, fmt.Errorf("%w: instance %q mounts under %q, another instance under %q", ErrNFSRequirementIncompatible, instanceKey, req.MountRoot, m.cfg.MountRoot)
		}
		if req.MountOptions != m.cfg.MountOptions {
			return nil, fmt.Errorf("%w: instance %q mounts with options %q, another instance with %q", ErrNFSRequirementIncompatible, instanceKey, req.MountOptions, m.cfg.MountOptions)
		}
	}
	var added []config.V1NFSShare
	for _, s := range req.Shares {
		existing, found := m.share(s.ID)
		if found {
			if existing.Server != s.Server || existing.Export != s.Export {
				return nil, fmt.Errorf("%w: share %q (mount path %s/%s) is %s:%s for instance %q but %s:%s for instance %q",
					ErrNFSRequirementIncompatible, s.ID, m.cfg.MountRoot, s.ID, s.Server, s.Export, instanceKey, existing.Server, existing.Export, m.sources[s.ID])
			}
			continue
		}
		added = append(added, s)
	}
	// Validated as a whole: only now change the union.
	for _, s := range added {
		m.cfg.Shares = append(m.cfg.Shares, s)
		m.sources[s.ID] = instanceKey
	}
	m.cfg.AutoMount = m.cfg.AutoMount || req.AutoMount
	if m.reconciler == nil {
		m.reconciler = NewNFSMountReconciler(m.cfg, m.checker, m.log)
	}
	return m.reconciler, nil
}

func (m *HostNFSMounter) share(id string) (config.V1NFSShare, bool) {
	if m.cfg == nil {
		return config.V1NFSShare{}, false
	}
	for _, s := range m.cfg.Shares {
		if s.ID == id {
			return s, true
		}
	}
	return config.V1NFSShare{}, false
}

// ShareIDs returns the shares of the union, sorted.
func (m *HostNFSMounter) ShareIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.sources))
	for id := range m.sources {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// FirstPassDone is closed once the loop's first reconcile pass finished
// (or at Stop when the loop never ran).
func (m *HostNFSMounter) FirstPassDone() <-chan struct{} { return m.firstPass }

// Start runs the reconcile loop over the union, once. With no registered
// requirement there is nothing to mount and no loop runs.
func (m *HostNFSMounter) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started {
		return nil
	}
	m.started = true
	if m.reconciler == nil {
		close(m.firstPass)
		close(m.stopped)
		return nil
	}
	if m.reconciler.MountsShares() {
		if err := m.reconciler.mountPrivilegeError(); err != nil {
			m.log.Warn("server.workspace_storage.nfs.auto_mount is on but the host cannot mount; shares are checked only", "reason", err)
		}
	}
	m.log.Info("Host NFS mounter starting", "shares", len(m.cfg.Shares), "mountRoot", m.cfg.MountRoot, "hostMounts", m.reconciler.MountsShares())
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	m.cancel = cancel
	r, interval := m.reconciler, m.interval
	go func() {
		defer close(m.stopped)
		var once sync.Once
		r.Run(loopCtx, interval, func() {
			if r.IsHealthy() {
				m.log.Info("NFS mounts checked at startup", "status", r.HealthCheckString(), "autoMount", r.AutoMount())
			} else {
				m.log.Error("NFS mounts unhealthy at startup; the instances keep serving", "detail", r.HealthCheckString(), "autoMount", r.AutoMount())
			}
			once.Do(func() { close(m.firstPass) })
		})
		once.Do(func() { close(m.firstPass) })
	}()
	return nil
}

// Stop ends the reconcile loop (a mount in progress is cancelled) and waits
// for it within ctx. It never unmounts anything.
func (m *HostNFSMounter) Stop(ctx context.Context) {
	m.mu.Lock()
	cancel, started := m.cancel, m.started
	if !started {
		m.started = true
		close(m.firstPass)
		close(m.stopped)
	}
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	select {
	case <-m.stopped:
	case <-ctx.Done():
	}
}
