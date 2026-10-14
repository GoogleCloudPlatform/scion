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

// Package brokerhost hosts a collection of explicitly configured flat
// Runtime Broker instances in one process (ptone/scion#3272): each instance
// has its own immutable key, persisted identity, runtime, manager,
// credentials and logger, and is activated or refused independently.
//
// Activation is two-pass. Pass 1 builds every configured instance's runtime,
// probes its execution scope and loads or creates its identity, with no Hub
// registration, heartbeat or control-channel side effect. Instances may share
// an execution scope (one Docker daemon, one Kubernetes cluster and
// namespace); each acts only on the runtime objects it owns. Unresolved
// ownership found by any instance's pass-1 check (its ownership preflight, or
// ownership records that cannot be read) refuses every instance on that
// scope before pass 2. Pass 2 activates the eligible instances: the Activator
// validates the Hub binding (the embedded registration when the Hub runs in
// the process, otherwise the remote activation validation), and only a bound
// result builds and starts the instance's Runtime Broker server.
//
// Flat instances stay within the hub.flat_runtime_brokers experiment, which
// the Hub enforces for every flat instance: a new flat instance's first
// registration and every create on a flat Runtime Broker are refused while it
// is off. A flat instance whose row already exists re-registers with it off.
//
// The host owns the process-wide HTTP listener. Its routing follows the
// CONFIGURED cardinality: with exactly one instance configured, the listener
// serves that instance (when active); with more than one configured, it
// serves host health only and never an instance route, whatever activates.
package brokerhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// Mode is how instances are activated, fixed for the process.
type Mode string

const (
	// ModeColocated: the Hub runs in this process; instances activate
	// through the embedded registration.
	ModeColocated Mode = "colocated"
	// ModeRemote: the Hub is elsewhere (or --simulate-remote-broker);
	// instances activate with their instance-scoped credentials after the
	// remote activation validation.
	ModeRemote Mode = "remote"
)

// State is an instance's lifecycle state.
type State string

const (
	StatePending State = "pending"
	StateActive  State = "active"
	StateRefused State = "refused"
	StateStopped State = "stopped"
)

// RuntimeFactory builds an instance's runtime from its own explicit
// configuration, never from Runtime Broker Profile resolution or process-wide
// environment switches.
type RuntimeFactory func(ctx context.Context, inst config.V1RuntimeBrokerInstanceConfig) (runtime.Runtime, error)

// ScopeProber probes the execution scope of an instance's runtime (Docker:
// the daemon ID through the runtime's own CLI).
type ScopeProber func(ctx context.Context, inst config.V1RuntimeBrokerInstanceConfig, rt runtime.Runtime) (brokeridentity.ExecutionScope, error)

// Activation is a bound activation result: the credentials the instance's
// server connects with.
type Activation struct {
	// InMemoryCredentials are the embedded registration's credentials
	// (ModeColocated).
	InMemoryCredentials *brokercredentials.BrokerCredentials
	// RemoteCredentials are the instance-scoped credentials that passed the
	// remote activation validation (ModeRemote).
	RemoteCredentials []brokercredentials.BrokerCredentials
}

// Candidate is an instance that passed pass 1 (runtime built, scope probed,
// identity loaded, ownership resolved on its scope).
type Candidate struct {
	Instance config.V1RuntimeBrokerInstanceConfig
	Identity *brokeridentity.Identity
	Runtime  runtime.Runtime
	// StateDir is the instance's broker state root
	// (runtimebroker.DefaultStateDir of its Runtime Broker ID), resolved
	// once by the host: its ownership records live there, and its server
	// uses the same directory (InstanceContext.StateDir).
	StateDir string
}

// Service is a host-level service (see Config.Services).
type Service interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context)
}

// Activator validates an instance's Hub binding. Activate returns a non-nil
// Activation only for a bound result; any error leaves the instance
// unactivated. Refused records a refusal (from any step) for reporting.
type Activator interface {
	Activate(ctx context.Context, c Candidate) (*Activation, error)
	Refused(inst config.V1RuntimeBrokerInstanceConfig, err error)
}

// InstanceContext is the immutable context of one activated instance,
// passed explicitly to everything it runs.
type InstanceContext struct {
	Instance   config.V1RuntimeBrokerInstanceConfig
	Identity   *brokeridentity.Identity
	Runtime    runtime.Runtime
	Manager    agent.Manager
	Activation *Activation
	// MultiInstance is true when more than one instance is configured.
	MultiInstance bool
	// StateDir is the instance's broker state root (Candidate.StateDir).
	StateDir string
	// ConflictingKeys are ownership keys this instance claims that another
	// configured instance claims too; the instance refuses operations on
	// them (unresolved ownership).
	ConflictingKeys map[string]bool
	// Logger carries the instance's broker_id and key.
	Logger *slog.Logger
}

// ServerBuilder builds an activated instance's Runtime Broker server; the
// caller supplies the process-wide parts of its configuration.
type ServerBuilder func(ic InstanceContext) (*runtimebroker.Server, error)

// ListenerConfig is the process-wide listener (server.broker.host/port).
type ListenerConfig struct {
	Host         string
	Port         int
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
}

// Config configures a Host.
type Config struct {
	GlobalDir string
	// Instances are the strictly loaded and validated
	// server.broker.instances, in configuration order.
	Instances   []config.V1RuntimeBrokerInstanceConfig
	Mode        Mode
	LegacyIDs   []string
	NewRuntime  RuntimeFactory
	ProbeScope  ScopeProber
	Activator   Activator
	BuildServer ServerBuilder
	// OwnershipPreflight, when set, runs in pass 1 for every candidate
	// after its scope and identity are established and before any Hub
	// activation: it inspects the instance's scope and refuses (returns an
	// error) when ownership there is unresolved, for example unlabeled
	// agent objects, or when the scope cannot be inspected. A refused
	// candidate is never activated.
	OwnershipPreflight func(ctx context.Context, c Candidate) error
	// OwnershipKeys, when set, returns a candidate's live ownership keys
	// (project ID + agent ID, and project ID + slug). After pass 1 the
	// host reports a key claimed by more than one configured instance as
	// conflicting to every instance that claims it
	// (InstanceContext.ConflictingKeys); those instances refuse operations
	// on that key, nothing else.
	OwnershipKeys func(ctx context.Context, c Candidate) ([]string, error)
	// Services are host-level services every instance depends on (the
	// single NFS mount owner): Run starts them, in order, before any
	// instance's services, and every shutdown stops them, in reverse
	// order, after every instance has shut down.
	Services []Service
	Listener ListenerConfig
	Logger   *slog.Logger
	// ShutdownTimeout bounds the whole shutdown; zero means
	// runtimebroker.ShutdownDeadline (30s, as for a single Runtime Broker).
	ShutdownTimeout time.Duration
}

// InstanceStatus is the reported state of one configured instance.
type InstanceStatus struct {
	Key             string `json:"key"`
	Name            string `json:"name"`
	RuntimeBrokerID string `json:"runtimeBrokerId,omitempty"`
	RuntimeTargetID string `json:"runtimeTargetId,omitempty"`
	State           State  `json:"state"`
	// Reason is a stable reason code for a refused or stopped instance
	// (for example ownership_unresolved, not_registered, runtime_target_ack_missing,
	// scope_unidentified). It is what health responses show.
	Reason string `json:"reason,omitempty"`
	// Error is the full refusal text for in-process callers and logs. It
	// can carry local detail (paths, endpoints, store errors), so it is
	// never serialized into a health response.
	Error string `json:"-"`
}

type instance struct {
	cfg      config.V1RuntimeBrokerInstanceConfig
	rt       runtime.Runtime
	identity *brokeridentity.Identity
	scope    brokeridentity.ExecutionScope
	// ownershipKeys are the instance's live ownership keys (pass 1).
	ownershipKeys []string
	// stateDir is the instance's broker state root (pass 1).
	stateDir string
	server   *runtimebroker.Server
	ctx      InstanceContext
	state    State
	err      error
}

// Host hosts the configured instances.
type Host struct {
	cfg       Config
	log       *slog.Logger
	mu        sync.RWMutex
	instances []*instance
	prepared  bool
	httpSrv   *http.Server
	addr      string
	// servicesStop stops the host services once.
	servicesStop sync.Once
}

// New validates cfg and returns a Host. Invalid instance configuration
// (invalid or duplicate keys, unsupported targets) refuses the whole process:
// no instance starts.
func New(cfg Config) (*Host, error) {
	if len(cfg.Instances) == 0 {
		return nil, errors.New("brokerhost: no Runtime Broker instances configured")
	}
	if errs := config.ValidateRuntimeBrokerInstances(cfg.Instances); len(errs) > 0 {
		msgs := make([]string, 0, len(errs))
		for _, e := range errs {
			msgs = append(msgs, e.Path+": "+e.Message)
		}
		return nil, fmt.Errorf("server.broker.instances: %s", strings.Join(msgs, "; "))
	}
	if cfg.Mode != ModeColocated && cfg.Mode != ModeRemote {
		return nil, fmt.Errorf("brokerhost: unknown mode %q", cfg.Mode)
	}
	if cfg.NewRuntime == nil || cfg.ProbeScope == nil || cfg.Activator == nil || cfg.BuildServer == nil {
		return nil, errors.New("brokerhost: NewRuntime, ProbeScope, Activator and BuildServer are required")
	}
	if cfg.GlobalDir == "" {
		return nil, errors.New("brokerhost: GlobalDir is required")
	}
	log := cfg.Logger
	if log == nil {
		log = logging.Subsystem("broker.host")
	}
	h := &Host{cfg: cfg, log: log}
	for _, inst := range cfg.Instances {
		h.instances = append(h.instances, &instance{cfg: inst, state: StatePending})
	}
	return h, nil
}

// MultiInstance reports whether more than one instance is configured. It is
// the configured cardinality, never the active count.
func (h *Host) MultiInstance() bool { return len(h.cfg.Instances) > 1 }

func (h *Host) refuse(in *instance, err error) {
	h.mu.Lock()
	in.state = StateRefused
	in.err = err
	h.mu.Unlock()
	h.log.Error("Runtime Broker instance not activated", "instance", in.cfg.Key, "error", err)
	h.cfg.Activator.Refused(in.cfg, err)
}

// Prepare runs both activation passes. It returns an error only when it was
// already run; per-instance refusals are reported through Status and the
// Activator, and never stop sibling instances.
func (h *Host) Prepare(ctx context.Context) error {
	h.mu.Lock()
	if h.prepared {
		h.mu.Unlock()
		return errors.New("brokerhost: already prepared")
	}
	h.prepared = true
	h.mu.Unlock()

	// Pass 1: runtime, scope and identity for every candidate. No Hub
	// registration, heartbeat or control channel. Prepare runs on one
	// goroutine and the Host is not shared until it returns, so the
	// per-candidate fields below are written without h.mu; state changes
	// visible through Status take it.
	var candidates []*instance
	// unresolvedScopes maps an execution scope to the instances whose
	// ownership on it is unresolved.
	unresolvedScopes := map[string][]string{}
	for _, in := range h.instances {
		rt, err := h.cfg.NewRuntime(ctx, in.cfg)
		if err != nil {
			h.refuse(in, &runtimeBuildError{err: fmt.Errorf("flat Runtime Broker instance %q: building its runtime: %w", in.cfg.Key, err)})
			continue
		}
		scope, err := h.cfg.ProbeScope(ctx, in.cfg, rt)
		if err != nil {
			h.refuse(in, fmt.Errorf("flat Runtime Broker instance %q: %w", in.cfg.Key, err))
			continue
		}
		id, err := brokeridentity.LoadOrCreate(brokeridentity.InstanceDir(h.cfg.GlobalDir, in.cfg.Key), in.cfg.Key, in.cfg.RuntimeTarget.Type, scope, h.cfg.LegacyIDs)
		if err != nil {
			h.refuse(in, fmt.Errorf("flat Runtime Broker instance %q: %w", in.cfg.Key, err))
			continue
		}
		stateDir, err := runtimebroker.DefaultStateDir(id.RuntimeBrokerID)
		if err != nil {
			h.refuse(in, fmt.Errorf("flat Runtime Broker instance %q: resolving its state directory: %w", in.cfg.Key, err))
			continue
		}
		if h.cfg.OwnershipPreflight != nil {
			if err := h.cfg.OwnershipPreflight(ctx, Candidate{Instance: in.cfg, Identity: id, Runtime: rt, StateDir: stateDir}); err != nil {
				h.refuse(in, WithReason("ownership_unresolved", fmt.Errorf("flat Runtime Broker instance %q: %w", in.cfg.Key, err)))
				unresolvedScopes[ScopeKey(scope)] = append(unresolvedScopes[ScopeKey(scope)], in.cfg.Key)
				continue
			}
		}
		var ownKeys []string
		if h.cfg.OwnershipKeys != nil {
			if ownKeys, err = h.cfg.OwnershipKeys(ctx, Candidate{Instance: in.cfg, Identity: id, Runtime: rt, StateDir: stateDir}); err != nil {
				h.refuse(in, WithReason("ownership_unresolved", fmt.Errorf("flat Runtime Broker instance %q: reading its ownership records: %w", in.cfg.Key, err)))
				unresolvedScopes[ScopeKey(scope)] = append(unresolvedScopes[ScopeKey(scope)], in.cfg.Key)
				continue
			}
		}
		in.rt, in.scope, in.identity, in.ownershipKeys, in.stateDir = rt, scope, id, ownKeys, stateDir
		candidates = append(candidates, in)
	}

	// Instances may share an execution scope: runtime inventory, workspaces,
	// mounts and caches are partitioned or coordinated by instance. Unresolved
	// ownership on a scope (an unlabeled or incompletely labelled object, or
	// unreadable records, of any instance) refuses EVERY configured instance
	// on that scope, decided here, before any instance activates; instances
	// on other scopes are unaffected. A failed scope probe refuses only its
	// own instance (it has no scope).
	var eligible []*instance
	for _, in := range candidates {
		if blocked := unresolvedScopes[ScopeKey(in.scope)]; len(blocked) > 0 {
			sort.Strings(blocked)
			h.refuse(in, WithReason("ownership_unresolved", fmt.Errorf(
				"flat Runtime Broker instance %q not activated: ownership on its execution scope %s is unresolved (instances %s); "+
					"drain or recreate the unresolved agents through the Runtime Broker that created them",
				in.cfg.Key, ScopeKey(in.scope), strings.Join(blocked, ", "))))
			continue
		}
		eligible = append(eligible, in)
	}

	// Ownership keys claimed by more than one instance that passed pass 1
	// (including instances refused for their scope's unresolved ownership,
	// whose records still exist) are conflicting for every claimant.
	conflicts := map[string]map[string]bool{} // instance key -> conflicting ownership keys
	{
		claims := map[string][]string{}
		for _, in := range candidates {
			for _, k := range in.ownershipKeys {
				claims[k] = append(claims[k], in.cfg.Key)
			}
		}
		for k, owners := range claims {
			if len(owners) < 2 {
				continue
			}
			h.log.Error("Ownership key claimed by several Runtime Broker instances; operations on it are refused", "key", k, "instances", owners)
			for _, o := range owners {
				if conflicts[o] == nil {
					conflicts[o] = map[string]bool{}
				}
				conflicts[o][k] = true
			}
		}
	}

	// Pass 2: activate each eligible instance independently.
	for _, in := range eligible {
		act, err := h.cfg.Activator.Activate(ctx, Candidate{Instance: in.cfg, Identity: in.identity, Runtime: in.rt, StateDir: in.stateDir})
		if err != nil {
			h.refuse(in, err)
			continue
		}
		if act == nil {
			h.refuse(in, fmt.Errorf("flat Runtime Broker instance %q: activation returned no credentials", in.cfg.Key))
			continue
		}
		ic := InstanceContext{
			Instance:        in.cfg,
			Identity:        in.identity,
			Runtime:         in.rt,
			Manager:         agent.NewManager(in.rt),
			Activation:      act,
			MultiInstance:   h.MultiInstance(),
			ConflictingKeys: conflicts[in.cfg.Key],
			StateDir:        in.stateDir,
			Logger: h.log.With(slog.String(logging.AttrBrokerID, in.identity.RuntimeBrokerID),
				slog.String("instance", in.cfg.Key)),
		}
		srv, err := h.cfg.BuildServer(ic)
		if err != nil {
			h.refuse(in, fmt.Errorf("flat Runtime Broker instance %q: %w", in.cfg.Key, err))
			continue
		}
		h.mu.Lock()
		in.server, in.ctx, in.state = srv, ic, StateActive
		h.mu.Unlock()
		ic.Logger.Info("Runtime Broker instance activated", "runtimeTarget", in.identity.RuntimeTarget.ID)
	}
	return nil
}

// ScopeKey is the identity part of an execution scope (contract section 5):
// the Docker daemon ID, or the Kubernetes cluster UID plus namespace.
func ScopeKey(s brokeridentity.ExecutionScope) string {
	switch {
	case s.Docker != nil:
		return "docker:" + s.Docker.DaemonID
	case s.Kubernetes != nil:
		return "kubernetes:" + s.Kubernetes.ClusterUID + "/" + s.Kubernetes.Namespace
	default:
		return s.Type + ":"
	}
}

// Status returns every configured instance's state, in configuration order.
func (h *Host) Status() []InstanceStatus {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]InstanceStatus, 0, len(h.instances))
	for _, in := range h.instances {
		st := InstanceStatus{Key: in.cfg.Key, Name: in.cfg.Name, State: in.state}
		if in.identity != nil {
			st.RuntimeBrokerID = in.identity.RuntimeBrokerID
			st.RuntimeTargetID = in.identity.RuntimeTarget.ID
		}
		if in.err != nil {
			st.Error = in.err.Error()
			st.Reason = ReasonCode(in.err)
		}
		out = append(out, st)
	}
	return out
}

// Ready reports whether every configured instance is active.
func (h *Host) Ready() bool {
	for _, st := range h.Status() {
		if st.State != StateActive {
			return false
		}
	}
	return true
}

// Active returns the activated instances' contexts and servers, in
// configuration order.
func (h *Host) Active() []ActiveInstance {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var out []ActiveInstance
	for _, in := range h.instances {
		if in.state == StateActive {
			out = append(out, ActiveInstance{Context: in.ctx, Server: in.server})
		}
	}
	return out
}

// ActiveInstance is one activated instance.
type ActiveInstance struct {
	Context InstanceContext
	Server  *runtimebroker.Server
}

// Instance returns the activated instance with exactly this Runtime Broker
// ID. There is no fallback to any other instance.
func (h *Host) Instance(runtimeBrokerID string) (*runtimebroker.Server, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, in := range h.instances {
		if in.state == StateActive && in.identity != nil && in.identity.RuntimeBrokerID == runtimeBrokerID {
			return in.server, true
		}
	}
	return nil, false
}

// InstancePathPrefix is the instance-qualified route prefix
// (ptone/scion#3273): "/instances/<runtimeBrokerID>" followed by the
// instance's own route. A host registers "<base>/instances/<id>" as an
// instance's endpoint when several instances are configured.
const InstancePathPrefix = "/instances/"

// InstancePrefix returns the route prefix of the instance with this Runtime
// Broker ID.
func InstancePrefix(runtimeBrokerID string) string { return InstancePathPrefix + runtimeBrokerID }

// Handler is the process-wide listener's handler.
//
//   - "/instances/<id>/...": the active instance with exactly that Runtime
//     Broker ID. Its prefixed handler verifies the signature with only that
//     instance's credentials over the full, prefixed path, then strips the
//     prefix. An unknown, refused or stopped ID is 404, and so is a request
//     whose Runtime Broker ID header names another broker; there is never a
//     fallback to another instance. The path is matched as sent: a
//     non-canonical path (encoded separators, dot segments, repeated
//     slashes) under the namespace is 404, never cleaned or redirected into
//     another route.
//   - Root routes follow the CONFIGURED cardinality: with exactly one
//     instance configured and active, the root serves that instance as an
//     unadvertised P1 compatibility alias, under the same Runtime Broker ID
//     binding; otherwise the root serves host health only and answers 404
//     for everything else, whatever activated.
func (h *Host) Handler() http.Handler {
	prefixed := map[string]http.Handler{}
	for _, a := range h.Active() {
		id := a.Context.Identity.RuntimeBrokerID
		prefixed[id] = a.Server.PrefixedHandler(InstancePrefix(id))
	}
	var root http.Handler
	if !h.MultiInstance() {
		if active := h.Active(); len(active) == 1 {
			root = bindBrokerID(active[0].Context.Identity.RuntimeBrokerID, active[0].Server.Handler())
		}
	}
	if root == nil {
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", h.handleHealthz)
		mux.HandleFunc("/readyz", h.handleReadyz)
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
		root = mux
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, InstancePathPrefix) && r.URL.Path != strings.TrimSuffix(InstancePathPrefix, "/") {
			root.ServeHTTP(w, r)
			return
		}
		id, ok := instanceRouteID(r)
		if !ok {
			http.NotFound(w, r)
			return
		}
		target, ok := prefixed[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		bindBrokerID(id, target).ServeHTTP(w, r)
	})
}

// bindBrokerID refuses (404) a request whose Runtime Broker ID header names
// a broker other than id, before any handler of id runs. A request without
// the header goes on to id's own authentication.
func bindBrokerID(id string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if signed := r.Header.Get(apiclient.HeaderBrokerID); signed != "" && signed != id {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// instanceRouteID returns the Runtime Broker ID a request under the
// instance namespace selects. The namespace and the ID segment are matched
// literally, as sent (no percent-encoding in the ID), and the path must have
// no empty, "." or ".." segment (sent or after decoding), so a request is
// never cleaned or redirected into another route. Percent-encoded data in
// the route suffix and the query is the instance's own route contract and
// passes through unchanged.
func instanceRouteID(r *http.Request) (string, bool) {
	escaped := r.URL.EscapedPath()
	rest, ok := strings.CutPrefix(escaped, InstancePathPrefix)
	if !ok {
		return "", false
	}
	id, _, _ := strings.Cut(rest, "/")
	if id == "" || strings.Contains(id, "%") {
		return "", false
	}
	if !cleanSegments(escaped, true) || !cleanSegments(r.URL.Path, false) {
		return "", false
	}
	return id, true
}

// cleanSegments reports whether path has no "." or ".." segment and, when
// noEmpty is set, no empty segment except a trailing one.
func cleanSegments(path string, noEmpty bool) bool {
	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, seg := range segments {
		if seg == "." || seg == ".." {
			return false
		}
		if noEmpty && seg == "" && i != len(segments)-1 {
			return false
		}
	}
	return true
}

type healthResponse struct {
	Status    string           `json:"status"`
	Instances []InstanceStatus `json:"instances"`
}

// handleHealthz is liveness: the host is serving. It reports each
// instance's state; it never implies the instances are ready.
func (h *Host) handleHealthz(w http.ResponseWriter, r *http.Request) {
	status := "ok"
	if !h.Ready() {
		status = "degraded"
	}
	writeJSON(w, http.StatusOK, healthResponse{Status: status, Instances: h.Status()})
}

// handleReadyz is readiness: 200 only when every configured instance is
// active; 503 while any is refused or pending.
func (h *Host) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if h.Ready() {
		writeJSON(w, http.StatusOK, healthResponse{Status: "ready", Instances: h.Status()})
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, healthResponse{Status: "not ready", Instances: h.Status()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Run starts every active instance's services and the process-wide
// listener, and blocks until ctx is done (then shuts everything down within
// the shutdown bound) or the listener fails. Prepare must have run.
//
// An instance whose services fail to start, or (with more than one instance
// configured) that ends up with no Hub connection, is marked stopped and
// reported refused: with several instances it is reachable only over its
// own control channel, so without one it is not ready.
func (h *Host) Run(ctx context.Context) error {
	h.mu.RLock()
	prepared := h.prepared
	h.mu.RUnlock()
	if !prepared {
		return errors.New("brokerhost: Run before Prepare")
	}
	for i, svc := range h.cfg.Services {
		if err := svc.Start(ctx); err != nil {
			sctx, cancel := context.WithTimeout(context.Background(), h.shutdownTimeout())
			defer cancel()
			for j := i - 1; j >= 0; j-- {
				h.cfg.Services[j].Stop(sctx)
			}
			return fmt.Errorf("brokerhost: starting a host service: %w", err)
		}
	}
	for _, a := range h.Active() {
		err := a.Server.StartServices(ctx)
		if err == nil && h.MultiInstance() && a.Server.HubConnectionCount() == 0 {
			err = fmt.Errorf("flat Runtime Broker instance %q has no Hub connection; with several instances configured it is reachable only over its control channel", a.Context.Instance.Key)
		}
		if err != nil {
			err = WithReason("not_serving", err)
			h.markStopped(a.Context.Identity.RuntimeBrokerID, err)
			a.Context.Logger.Error("Runtime Broker instance not serving", "error", err)
			h.cfg.Activator.Refused(a.Context.Instance, err)
			// Stop whatever the instance did start (hub connections).
			sctx, cancel := context.WithTimeout(context.Background(), h.shutdownTimeout())
			_ = a.Server.Shutdown(sctx)
			cancel()
		}
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", h.cfg.Listener.Host, h.cfg.Listener.Port))
	if err != nil {
		h.shutdownInstances(ctx)
		return err
	}
	handler := h.Handler()
	h.mu.Lock()
	h.httpSrv = &http.Server{
		Handler:      handler,
		ReadTimeout:  h.cfg.Listener.ReadTimeout,
		WriteTimeout: h.cfg.Listener.WriteTimeout,
	}
	h.addr = ln.Addr().String()
	srv := h.httpSrv
	h.mu.Unlock()

	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()
	select {
	case err, ok := <-errCh:
		sctx, cancel := context.WithTimeout(context.Background(), h.shutdownTimeout())
		defer cancel()
		h.shutdownInstances(sctx)
		if !ok {
			return nil
		}
		return err
	case <-ctx.Done():
		return h.Shutdown(context.Background())
	}
}

// Addr is the listener's address once Run has started it ("" before).
func (h *Host) Addr() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.addr
}

// shutdownTimeout is the bound on the whole shutdown (instances and the
// listener drain): runtimebroker.ShutdownDeadline unless configured.
func (h *Host) shutdownTimeout() time.Duration {
	if h.cfg.ShutdownTimeout > 0 {
		return h.cfg.ShutdownTimeout
	}
	return runtimebroker.ShutdownDeadline
}

func (h *Host) markStopped(runtimeBrokerID string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, in := range h.instances {
		if in.identity != nil && in.identity.RuntimeBrokerID == runtimeBrokerID {
			in.state, in.err = StateStopped, err
		}
	}
}

// Shutdown stops every active instance (each drains its own starts in
// flight), then drains the listener. One deadline bounds both: ctx's, or
// the shutdown bound when ctx has none, so one open request can never keep
// the process from exiting.
func (h *Host) Shutdown(ctx context.Context) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, h.shutdownTimeout())
		defer cancel()
	}
	h.shutdownInstances(ctx)
	h.mu.RLock()
	srv := h.httpSrv
	h.mu.RUnlock()
	if srv == nil {
		return nil
	}
	if err := srv.Shutdown(ctx); err != nil {
		_ = srv.Close()
		return err
	}
	return nil
}

func (h *Host) shutdownInstances(ctx context.Context) {
	defer h.stopServices(ctx)
	var wg sync.WaitGroup
	for _, a := range h.Active() {
		wg.Add(1)
		go func(a ActiveInstance) {
			defer wg.Done()
			if err := a.Server.Shutdown(ctx); err != nil {
				a.Context.Logger.Warn("Runtime Broker instance shutdown", "error", err)
			}
		}(a)
	}
	wg.Wait()
	h.mu.Lock()
	for _, in := range h.instances {
		if in.state == StateActive {
			in.state = StateStopped
		}
	}
	h.mu.Unlock()
}

// stopServices stops the host services, in reverse order, once (after the
// instances that depend on them have shut down).
func (h *Host) stopServices(ctx context.Context) {
	h.servicesStop.Do(func() {
		for i := len(h.cfg.Services) - 1; i >= 0; i-- {
			h.cfg.Services[i].Stop(ctx)
		}
	})
}

// ReasonError attaches a stable reason code to a refusal (see
// InstanceStatus.Reason).
type ReasonError struct {
	Reason string
	Err    error
}

func (e *ReasonError) Error() string { return e.Err.Error() }
func (e *ReasonError) Unwrap() error { return e.Err }

// WithReason wraps err with a stable reason code.
func WithReason(reason string, err error) error {
	if err == nil {
		return nil
	}
	return &ReasonError{Reason: reason, Err: err}
}

type runtimeBuildError struct{ err error }

func (e *runtimeBuildError) Error() string { return e.err.Error() }
func (e *runtimeBuildError) Unwrap() error { return e.err }

// ReasonCode classifies a refusal into a stable reason code for health
// responses; the full error text stays in logs and Status().Error.
func ReasonCode(err error) string {
	if err == nil {
		return ""
	}
	var re *ReasonError
	if errors.As(err, &re) && re.Reason != "" {
		return re.Reason
	}
	var ack *brokeridentity.AckError
	if errors.As(err, &ack) && ack.Code != "" {
		return ack.Code
	}
	var coded interface{ Code() string }
	if errors.As(err, &coded) && coded.Code() != "" {
		return coded.Code()
	}
	switch {
	case errors.Is(err, brokeridentity.ErrExecutionScopeUnidentified):
		return "scope_unidentified"
	case errors.Is(err, brokeridentity.ErrExecutionScopeChanged):
		return "scope_changed"
	case errors.Is(err, brokeridentity.ErrRuntimeTargetTypeChanged):
		return "runtime_target_type_changed"
	case errors.Is(err, brokeridentity.ErrIdentityMissing), errors.Is(err, brokeridentity.ErrIdentityCorrupt),
		errors.Is(err, brokeridentity.ErrIdentityKeyMismatch), errors.Is(err, brokeridentity.ErrIdentityCollidesWithLegacy):
		return "identity_error"
	}
	var rb *runtimeBuildError
	if errors.As(err, &rb) {
		return "runtime_build_failed"
	}
	return "refused"
}
