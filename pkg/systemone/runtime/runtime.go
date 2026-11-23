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

package runtime

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/textproto"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/systemone/egress"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
)

// errRuntimeDeadline is the sentinel cause attached via context.WithDeadlineCause
// to every runtime-derived deadline so Evaluate and ListModels deterministically
// distinguish runtime-owned timeouts (FailTimeout) from caller context
// termination (FailCanceled) by cause rather than wall-clock timestamps
// (runtime-design §§3, 6.2, 7.1).
var errRuntimeDeadline = errors.New("systemone runtime: evaluation deadline exceeded")

// CredentialSource resolves the raw bearer API key (without the "Bearer "
// prefix) for inst at request time. Implementations (such as Hub secret-store
// wiring) must never log or echo secret values. A missing, deleted, or rejected
// secret, or an empty/invalid token, must return or wrap egress.ErrAuth
// (classified as auth); any other backend fault must return or wrap
// egress.ErrCredentialUnavailable (classified as unavailable). Runtime passes a
// bound egress.BearerSource to egress.NewClient and never exposes credentials
// to Adapter methods or ModelRequest (runtime-design §4.3, AC-2).
type CredentialSource func(ctx context.Context, inst Instance) (string, error)

// StaticCredential returns a CredentialSource that yields key on every call.
func StaticCredential(key string) CredentialSource {
	return func(context.Context, Instance) (string, error) {
		return key, nil
	}
}

// InstanceSource resolves configured System One instances without coupling
// pkg/systemone/runtime to pkg/store or pkg/hub.
type InstanceSource interface {
	GetInstance(ctx context.Context, name string) (Instance, bool, error)
	ListInstances(ctx context.Context) ([]Instance, error)
}

type staticInstanceSource struct {
	byName map[string]Instance
	order  []Instance
}

// StaticInstances constructs an in-memory InstanceSource from instances.
func StaticInstances(instances ...Instance) InstanceSource {
	byName := make(map[string]Instance, len(instances))
	order := make([]Instance, 0, len(instances))
	for _, inst := range instances {
		cp := cloneInstance(inst)
		byName[cp.Name] = cp
		order = append(order, cp)
	}
	return &staticInstanceSource{byName: byName, order: order}
}

func (s *staticInstanceSource) GetInstance(_ context.Context, name string) (Instance, bool, error) {
	inst, ok := s.byName[name]
	if !ok {
		return Instance{}, false, nil
	}
	return cloneInstance(inst), true, nil
}

func (s *staticInstanceSource) ListInstances(_ context.Context) ([]Instance, error) {
	out := make([]Instance, len(s.order))
	for i, inst := range s.order {
		out[i] = cloneInstance(inst)
	}
	return out, nil
}

// ProjectAllowlistSource resolves the per-project System One instance allowlist
// without coupling pkg/systemone/runtime to pkg/store or pkg/hub
// (t2-p2-trigger-binding-amendment D1; runtime-design §4.5 rule 4).
//
// Returning an empty slice means no hub instances are allowed for projectID
// (default-empty everywhere, including workstation hubs).
type ProjectAllowlistSource interface {
	AllowedInstances(ctx context.Context, projectID string) ([]string, error)
}

type staticProjectAllowlist struct {
	mu      sync.RWMutex
	byProj  map[string][]string
}

// StaticProjectAllowlist constructs an in-memory ProjectAllowlistSource from a
// map of projectID -> allowed instance names.
func StaticProjectAllowlist(allowed map[string][]string) ProjectAllowlistSource {
	cp := make(map[string][]string, len(allowed))
	for k, v := range allowed {
		cp[k] = slices.Clone(v)
	}
	return &staticProjectAllowlist{byProj: cp}
}

func (s *staticProjectAllowlist) AllowedInstances(_ context.Context, projectID string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.byProj[strings.TrimSpace(projectID)]), nil
}

// Config configures a Runtime executor.
type Config struct {
	// Registry is the provider Adapter registry. Defaults to provider.DefaultRegistry() when nil.
	Registry *provider.Registry
	// Instances resolves configured provider instances. Optional when callers
	// supply EvalOptions.Instance directly.
	Instances InstanceSource
	// Credentials supplies per-instance bearer/header API keys at request time
	// for API-key authentication modes.
	Credentials CredentialSource
	// TokenSource supplies per-instance OAuth/ADC access tokens at request time
	// when Instance.Auth.Kind is "google_access_token". When nil,
	// google_access_token evaluations fail closed with egress.ErrAuth (0 provider
	// requests) and never fall back to Credentials.
	TokenSource CredentialSource
	// AllowedSecretNames optionally overrides how Runtime resolves the exact
	// provider-pinned named-secret allowlist for an Adapter. When nil, Runtime
	// uses provider.AllowedSecretNamesFor (failing closed when an adapter does
	// not implement provider.SecretNamePolicy).
	AllowedSecretNames func(adapter provider.Adapter) []string
	// MaxEvalTime is the runtime-wide evaluation ceiling (server.hub.systemone.maxEvalTime;
	// defaults to DefaultMaxEvalTime = 3s when <= 0, clamped to MaxAllowedEvalTime = 30s).
	MaxEvalTime time.Duration
	// MaxInFlightPerProject is the per-replica concurrency limit per project
	// (server.hub.system_one.max_in_flight_per_project; runtime-design §§4.2, 7.2).
	// <= 0 disables the per-project concurrency cap (unlimited, matching unset
	// InstanceLimits.MaxInFlight); the Hub sets this to
	// DefaultMaxInFlightPerProject (4) by default.
	MaxInFlightPerProject int
	// AdmissionWait is the maximum time Evaluate waits to acquire per-project
	// and per-instance concurrency/rate admission permits before failing fast
	// with FailCapacity (runtime-design §7.2). <= 0 uses DefaultAdmissionWait (100ms),
	// clamped to MaxAllowedEvalTime.
	AdmissionWait time.Duration
	// ProjectAllowlist optionally enforces the per-project hub instance allowlist
	// at authoring (ValidateCompiled) and evaluation (Evaluate, ListModels)
	// before any credential resolution or provider I/O (D1).
	ProjectAllowlist ProjectAllowlistSource
	// DefaultInstance optionally names the fallback default instance when a
	// portable rule omits Spec.Binding.Instance.
	DefaultInstance string
	// Logger receives content-free debug provenance lines. Defaults to slog.Default().
	Logger *slog.Logger
}

// Option configures test seams on New. Production callers pass no options.
type Option func(*Runtime)

// WithEgressOptionsForTesting forwards egress test-seam options (such as
// egress.WithLoopbackServerForTesting or egress.WithDialerForTesting) into
// egress.NewClient during offline tests. Production callers pass no options.
func WithEgressOptionsForTesting(opts ...egress.Option) Option {
	cp := append([]egress.Option(nil), opts...)
	return func(r *Runtime) {
		r.egressOpts = append(r.egressOpts, cp...)
	}
}

// EvalOptions carries per-evaluation metadata and optional execution overrides.
type EvalOptions struct {
	// Instance optionally pins evaluation to an already-loaded Instance (used by
	// the Hub admin probe endpoint). If compiled.Spec.Binding.Instance is also
	// non-empty, the names must match or Evaluate fails with FailNoEligibleInstance.
	Instance *Instance
	// Consumer, RuleID, Revision, and optional ElementIndex populate Evaluation.Rule.
	Consumer     string
	RuleID       string
	Revision     int64
	ElementIndex *int
	// ProjectID populates Evaluation.ProjectID. It MUST come from the
	// consumer's authorized context, never untrusted caller input.
	ProjectID string
	// RequireProject instructs Evaluate to fail closed with
	// FailNoEligibleInstance (0 provider requests, 0 secret reads) when
	// ProjectID is empty and ProjectAllowlist is configured. Consumer paths that
	// evaluate project content set this true; hub-admin probes without --project
	// leave it false.
	RequireProject bool
	// MaxEvalTime optionally lowers MaxEvalTime for this evaluation (clamped to
	// MaxAllowedEvalTime = 30s; effective ceiling is min(r.MaxEvalTime(),
	// opts.MaxEvalTime), never raising above the Runtime's configured ceiling).
	MaxEvalTime time.Duration
	// Budget optionally caps the evaluation by a remaining aggregate consumer
	// budget when positive (runtime-design §7.1). Budget <= 0 means no budget is
	// set; callers tracking an aggregate budget must enforce exhaustion before
	// calling Evaluate and must never call Evaluate with an exhausted budget.
	Budget time.Duration
	// EngineMaxEvalTime optionally carries ruleengine.Budget.MaxEvalTime when
	// Evaluate is invoked through the ruleengine Dialect (P1 brief §5). When
	// positive, Runtime folds max(0, EngineMaxEvalTime - EngineDeadlineMargin)
	// into its own deadline so errRuntimeDeadline fires before ruleengine's
	// context deadline and passes through unchanged as FailTimeout.
	EngineMaxEvalTime time.Duration
	// EngineDeadlineMargin is the safety margin computed by
	// ruleengine.EngineDeadlineMargin(EngineMaxEvalTime), set by the systemone
	// Dialect alongside EngineMaxEvalTime. Only meaningful when
	// EngineMaxEvalTime > 0; negative values are treated as 0.
	EngineDeadlineMargin time.Duration
}

type transportKey struct {
	id               string
	name             string
	revision         int64
	adapter          string
	endpoint         string
	authKind         string
	secretName       string
	isNamedSecretRef bool
	secretScope      string
	headerName       string
	audience         string
	scopes           string
	quotaProject     string
	placementKind    provider.AuthKind
	placementHeader  string
}

type evalCallTrackerKey struct{}
type evalProjectIDKey struct{}

// WithEvalProjectID attaches a validated evaluation project ID to ctx so
// CredentialSource implementations resolving project-scoped named secrets
// receive the evaluation's own project over shared transports (PV3 Addendum F).
func WithEvalProjectID(ctx context.Context, projectID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, evalProjectIDKey{}, strings.TrimSpace(projectID))
}

// EvalProjectIDFromContext returns the validated evaluation project ID attached
// by Runtime.Evaluate or WithEvalProjectID, or "" when unset.
func EvalProjectIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(evalProjectIDKey{}).(string)
	return v
}

type evalCallTracker struct {
	preCallAuthFailed  atomic.Bool
	mu                 sync.Mutex
	credType           string
	saEmail            string
	secretRefName      string
	secretRefScopeUsed string
	secretVersion      int
}

// RecordAuthIdentity records non-secret credential category metadata (such as
// Google ADC credential type and service-account email) on the active
// evaluation's content-free provenance record. Values failing identifier
// sanitization are silently ignored; secret or token material must never be
// passed here.
func RecordAuthIdentity(ctx context.Context, credentialType, serviceAccountEmail string) {
	if ctx == nil {
		return
	}
	tracker, ok := ctx.Value(evalCallTrackerKey{}).(*evalCallTracker)
	if !ok || tracker == nil {
		return
	}
	cleanType, _ := sanitizeProviderIdentifier(credentialType)
	cleanEmail, _ := sanitizeProviderIdentifier(serviceAccountEmail)
	tracker.mu.Lock()
	if cleanType != "" {
		tracker.credType = cleanType
	}
	if cleanEmail != "" {
		tracker.saEmail = cleanEmail
	}
	tracker.mu.Unlock()
}

func (t *evalCallTracker) authIdentity() (string, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.credType, t.saEmail
}

// RecordSecretRefProvenance records non-secret named-secret resolution metadata
// (secret name, effective scope used: "project" or "hub", and positive metadata
// version) on the active evaluation's content-free provenance record. Values
// failing grammar/enum checks are silently ignored; secret values must never be
// passed here.
func RecordSecretRefProvenance(ctx context.Context, name, scopeUsed string, version int) {
	if ctx == nil {
		return
	}
	tracker, ok := ctx.Value(evalCallTrackerKey{}).(*evalCallTracker)
	if !ok || tracker == nil {
		return
	}
	if !provider.IsValidSecretRefName(name) {
		return
	}
	if scopeUsed != provider.SecretScopeHub && scopeUsed != provider.SecretScopeProject {
		return
	}
	tracker.mu.Lock()
	tracker.secretRefName = name
	tracker.secretRefScopeUsed = scopeUsed
	if version > 0 {
		tracker.secretVersion = version
	}
	tracker.mu.Unlock()
}

func (t *evalCallTracker) secretRefProvenance() (string, string, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.secretRefName, t.secretRefScopeUsed, t.secretVersion
}

func markPreCallAuthFailed(ctx context.Context) {
	if ctx == nil {
		return
	}
	if tracker, ok := ctx.Value(evalCallTrackerKey{}).(*evalCallTracker); ok && tracker != nil {
		tracker.preCallAuthFailed.Store(true)
	}
}

// Runtime executes compiled System One rules under bounded deadlines and
// content-free provenance records (runtime-design §§6, 7.1, 7.7, 8.1).
type Runtime struct {
	registry           *provider.Registry
	instances          InstanceSource
	projectAllowlist   ProjectAllowlistSource
	credentials        CredentialSource
	tokenSource        CredentialSource
	allowedSecretNames func(provider.Adapter) []string
	maxEvalTime        time.Duration
	admission          *admissionController
	defaultInstance    string
	logger             *slog.Logger
	egressOpts         []egress.Option

	transportMu sync.Mutex
	transports  map[transportKey]provider.Transport
}

// New constructs a Runtime from cfg and optional test-seam options.
func New(cfg Config, opts ...Option) *Runtime {
	reg := cfg.Registry
	if reg == nil {
		reg = provider.DefaultRegistry()
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	maxEval := DefaultMaxEvalTime
	if cfg.MaxEvalTime > 0 {
		maxEval = clampDuration(cfg.MaxEvalTime, MaxAllowedEvalTime)
	}
	maxPerProject := cfg.MaxInFlightPerProject
	if maxPerProject < 0 {
		maxPerProject = 0 // unlimited
	}
	admissionWait := DefaultAdmissionWait
	if cfg.AdmissionWait > 0 {
		admissionWait = clampDuration(cfg.AdmissionWait, MaxAllowedEvalTime)
	}

	allowedNames := cfg.AllowedSecretNames
	if allowedNames == nil {
		allowedNames = provider.AllowedSecretNamesFor
	}

	r := &Runtime{
		registry:           reg,
		instances:          cfg.Instances,
		projectAllowlist:   cfg.ProjectAllowlist,
		credentials:        cfg.Credentials,
		tokenSource:        cfg.TokenSource,
		allowedSecretNames: allowedNames,
		maxEvalTime:        maxEval,
		admission:          newAdmissionController(maxPerProject, admissionWait),
		defaultInstance:    cfg.DefaultInstance,
		logger:             logger,
		transports:         make(map[transportKey]provider.Transport),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(r)
		}
	}
	return r
}

// MaxEvalTime returns the Runtime's configured evaluation time ceiling.
func (r *Runtime) MaxEvalTime() time.Duration {
	return r.maxEvalTime
}

// ValidateCompiled validates compiled at authoring/compile time against the
// bound or eligible instance's capabilities (primitive limits, conservative
// bytes-to-tokens MaxInputTokens pre-check, and Adapter.Validate;
// runtime-design §§4.4, 4.5).
//
// When r.instances is nil, ValidateCompiled is a no-op (for runtimes that
// supply EvalOptions.Instance per call). When compiled binds a specific
// instance, that instance must exist, be enabled, and pass validation. When
// compiled is unbound and enabled instances exist, at least one eligible
// instance must pass (or r.defaultInstance when configured).
func (r *Runtime) ValidateCompiled(ctx context.Context, compiled *spec.Compiled) error {
	if r == nil || compiled == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if compiled.Spec.Binding.Pool != "" {
		return &spec.CompileError{
			Path:     "spec.binding.pool",
			Category: spec.CategoryBinding,
			Message:  fmt.Sprintf("pool binding %q is not supported yet (deferred to P4)", compiled.Spec.Binding.Pool),
		}
	}
	if r.instances == nil {
		return nil
	}

	projectID := strings.TrimSpace(EvalProjectIDFromContext(ctx))
	var allowedSet map[string]struct{}
	checkAllowlist := projectID != "" && r.projectAllowlist != nil
	if checkAllowlist {
		allowed, err := r.projectAllowlist.AllowedInstances(ctx, projectID)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return &spec.CompileError{
				Path:     "spec.binding",
				Category: spec.CategoryBinding,
				Message:  fmt.Sprintf("failed to resolve allowed System One instances for project %q", projectID),
			}
		}
		allowedSet = make(map[string]struct{}, len(allowed))
		for _, name := range allowed {
			allowedSet[name] = struct{}{}
		}
	}

	var candidates []Instance
	switch {
	case compiled.Spec.Binding.Instance != "":
		if checkAllowlist {
			if _, ok := allowedSet[compiled.Spec.Binding.Instance]; !ok {
				return &spec.CompileError{
					Path:     "spec.binding.instance",
					Category: spec.CategoryBinding,
					Message:  fmt.Sprintf("instance %q is not allowed for project %q", compiled.Spec.Binding.Instance, projectID),
				}
			}
		}
		inst, found, err := r.instances.GetInstance(ctx, compiled.Spec.Binding.Instance)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return &spec.CompileError{
				Path:     "spec.binding.instance",
				Category: spec.CategoryBinding,
				Message:  fmt.Sprintf("failed to resolve instance %q", compiled.Spec.Binding.Instance),
			}
		}
		if !found || !inst.Enabled {
			return &spec.CompileError{
				Path:     "spec.binding.instance",
				Category: spec.CategoryBinding,
				Message:  fmt.Sprintf("instance %q is not found or disabled", compiled.Spec.Binding.Instance),
			}
		}
		candidates = []Instance{inst}

	case r.defaultInstance != "":
		if checkAllowlist {
			if _, ok := allowedSet[r.defaultInstance]; !ok {
				return &spec.CompileError{
					Path:     "spec.binding",
					Category: spec.CategoryBinding,
					Message:  fmt.Sprintf("default instance %q is not allowed for project %q", r.defaultInstance, projectID),
				}
			}
		}
		inst, found, err := r.instances.GetInstance(ctx, r.defaultInstance)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return &spec.CompileError{
				Path:     "spec.binding",
				Category: spec.CategoryBinding,
				Message:  fmt.Sprintf("failed to resolve default instance %q", r.defaultInstance),
			}
		}
		if !found || !inst.Enabled {
			return &spec.CompileError{
				Path:     "spec.binding",
				Category: spec.CategoryBinding,
				Message:  fmt.Sprintf("default instance %q is not found or disabled", r.defaultInstance),
			}
		}
		candidates = []Instance{inst}

	default:
		all, err := r.instances.ListInstances(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return &spec.CompileError{
				Path:     "spec.binding",
				Category: spec.CategoryBinding,
				Message:  "failed to list instances for compile validation",
			}
		}
		for _, inst := range all {
			if !inst.Enabled {
				continue
			}
			if checkAllowlist {
				if _, ok := allowedSet[inst.Name]; !ok {
					continue
				}
			}
			candidates = append(candidates, inst)
		}
		if len(candidates) == 0 {
			if checkAllowlist {
				return &spec.CompileError{
					Path:     "spec.binding",
					Category: spec.CategoryBinding,
					Message:  fmt.Sprintf("no enabled System One instance is allowed for project %q", projectID),
				}
			}
			return nil
		}
	}

	var firstErr error
	for _, inst := range candidates {
		if err := r.validateCompiledForInstance(compiled, inst); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		return nil
	}
	return firstErr
}

func (r *Runtime) validateCompiledForInstance(compiled *spec.Compiled, inst Instance) error {
	adapter, ok := r.registry.Get(inst.Adapter)
	if !ok {
		return &spec.CompileError{
			Path:     "spec.binding.instance",
			Category: spec.CategoryBinding,
			Message:  fmt.Sprintf("adapter %q for instance %q is not registered", inst.Adapter, inst.Name),
		}
	}
	view := inst.View()
	caps, err, panicked := capabilitiesSafely(adapter, view)
	if panicked || err != nil {
		return &spec.CompileError{
			Path:     "spec.binding.instance",
			Category: spec.CategoryBinding,
			Message:  fmt.Sprintf("failed to inspect capabilities for instance %q", inst.Name),
		}
	}
	if !caps.SideEffectFree {
		return &spec.CompileError{
			Path:     "spec.binding.instance",
			Category: spec.CategoryCapability,
			Message:  fmt.Sprintf("adapter %q for instance %q is not side-effect-free", inst.Adapter, inst.Name),
		}
	}

	specLimits := caps.SpecLimits()
	if inst.Limits.MaxInputTokens > 0 && (specLimits.MaxInputTokens <= 0 || inst.Limits.MaxInputTokens < specLimits.MaxInputTokens) {
		specLimits.MaxInputTokens = inst.Limits.MaxInputTokens
	}
	if err := spec.ValidateInputTokenBudget(compiled.Inputs, compiled.Environment, specLimits); err != nil {
		return err
	}
	spCopy := cloneSpec(compiled.Spec)
	if err := spec.ValidatePortable(&spCopy, specLimits); err != nil {
		return err
	}
	if err := adapter.Validate(view, spCopy); err != nil {
		if _, ok := spec.AsCompileError(err); ok {
			return err
		}
		return &spec.CompileError{
			Path:     "spec",
			Category: spec.CategoryCapability,
			Message:  fmt.Sprintf("instance %q rejected rule: %v", inst.Name, err),
		}
	}
	return nil
}

// CloseIdleConnections closes idle HTTP keep-alive connections across cached
// egress transports.
func (r *Runtime) CloseIdleConnections() {
	r.transportMu.Lock()
	defer r.transportMu.Unlock()
	for _, tr := range r.transports {
		tr.CloseIdleConnections()
	}
}

// ForgetInstance evicts any cached egress transports for the given instance ID
// regardless of revision, endpoint, or credential metadata, closing their idle
// HTTP connections. A blank id is a no-op.
func (r *Runtime) ForgetInstance(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	r.admission.forgetInstance(id)
	r.transportMu.Lock()
	defer r.transportMu.Unlock()
	for key, tr := range r.transports {
		if key.id == id {
			tr.CloseIdleConnections()
			delete(r.transports, key)
		}
	}
}

// Evaluate executes a single bounded evaluation of compiled over input with no
// retries, no fallback, and no caching (P1 scope; runtime-design §§6, 7.1, 8.1).
//
// On ClassSuccess, Evaluate returns (Decision{..., Eval: eval}, nil).
// On ClassUncertain or ClassFailed, Evaluate returns
// (Decision{Kind: compiled.Kind, Eval: eval}, &EvaluationError{Eval: eval}).
func (r *Runtime) Evaluate(ctx context.Context, compiled *spec.Compiled, input Input, opts EvalOptions) (Decision, error) {
	start := time.Now()
	evalID := newEvalID()

	if ctx == nil {
		ctx = context.Background()
	}

	var elemIdx *int
	if opts.ElementIndex != nil {
		v := *opts.ElementIndex
		elemIdx = &v
	}

	if compiled == nil {
		eval := &Evaluation{
			EvalID:    evalID,
			Rule:      RuleRef{Consumer: opts.Consumer, RuleID: opts.RuleID, Revision: opts.Revision, ElementIndex: elemIdx, Dialect: DialectName},
			ProjectID: opts.ProjectID,
			Class:     ClassFailed,
			Reason:    string(provider.FailInternal),
			Elapsed:   time.Since(start),
		}
		r.logEvaluation(ctx, eval)
		return Decision{Eval: eval}, &EvaluationError{Eval: eval}
	}

	eval := &Evaluation{
		EvalID: evalID,
		Rule: RuleRef{
			Consumer:     opts.Consumer,
			RuleID:       opts.RuleID,
			Revision:     opts.Revision,
			ElementIndex: elemIdx,
			ContentHash:  compiled.ContentHash,
			Dialect:      DialectName,
			Native:       compiled.Spec.Form == spec.FormNative,
		},
		ProjectID: opts.ProjectID,
		Requested: compiled.Spec.Binding,
	}

	// A pre-canceled caller context always ends immediately as FailCanceled
	// without computing field fingerprints or performing network I/O.
	if err := ctx.Err(); err != nil {
		return r.finishFailed(ctx, compiled.Kind, start, eval, provider.FailCanceled)
	}

	// Compose initial runtime-owned deadline (rule timeout, MaxEvalTime, Budget,
	// and EngineMaxEvalTime minus EngineDeadlineMargin) before instance resolution;
	// CallTimeout tightens it once inst is resolved. Latch errRuntimeDeadline
	// across stages so once the runtime's own deadline fires, the outcome stays
	// FailTimeout even if the parent/engine context also expires afterwards.
	baseOwnDur := r.effectiveOwnDuration(compiled.Timeout, opts, 0)
	ownDeadline := start.Add(baseOwnDur)
	runCtx, runCancel := context.WithDeadlineCause(ctx, ownDeadline, errRuntimeDeadline)
	defer runCancel()

	runtimeTimedOut := false
	latchCause := func(extraCauses ...error) error {
		if runtimeTimedOut {
			return errRuntimeDeadline
		}
		for _, c := range extraCauses {
			if errors.Is(c, errRuntimeDeadline) {
				runtimeTimedOut = true
				return errRuntimeDeadline
			}
		}
		rc := context.Cause(runCtx)
		if errors.Is(rc, errRuntimeDeadline) {
			runtimeTimedOut = true
			return errRuntimeDeadline
		}
		if len(extraCauses) > 0 && extraCauses[0] != nil {
			return extraCauses[0]
		}
		return rc
	}

	if runCtx.Err() != nil || ctx.Err() != nil {
		return r.finishFailed(ctx, compiled.Kind, start, eval, classifyContextCause(ctx.Err(), latchCause()))
	}

	// Step 1: Select and validate bounded content-environment fields and dynamic choice sets (AC-2, AC-3).
	selectedFields, selectedChoiceSets, fp, totalBytes, fieldReason := selectInputFields(compiled, input)
	eval.Input = fp
	if fieldReason != "" {
		return r.finishFailed(ctx, compiled.Kind, start, eval, fieldReason)
	}

	// Step 2: Resolve eligible instance and adapter (runtime-design §4.5).
	inst, adapter, caps, resolveReason := r.resolveBinding(runCtx, ctx, compiled, opts)
	if resolveReason != "" {
		if resolveReason == provider.FailCanceled && latchCause() == errRuntimeDeadline {
			resolveReason = provider.FailTimeout
		}
		return r.finishFailed(ctx, compiled.Kind, start, eval, resolveReason)
	}
	_ = latchCause()

	eval.ConfiguredModel = inst.Model

	// Step 2b: Check dynamic choice set item counts against the bound instance's
	// choice primitive MaxOptions (runtime-design §4.4).
	if csReason := checkChoiceSetInstanceLimits(selectedChoiceSets, caps); csReason != "" {
		return r.finishFailed(ctx, compiled.Kind, start, eval, csReason)
	}

	// Step 3: Conservative bytes-to-tokens pre-check against MaxInputTokens (AC-3).
	if precheckReason := checkMaxInputTokensBytes(totalBytes, caps, inst.Limits); precheckReason != "" {
		return r.finishFailed(ctx, compiled.Kind, start, eval, precheckReason)
	}

	// Step 4: Verify P1 single-request question limit and auth scheme support.
	if caps.MaxQuestionsPerRequest > 0 && len(compiled.Spec.Questions) > caps.MaxQuestionsPerRequest {
		return r.finishFailed(ctx, compiled.Kind, start, eval, provider.FailUnsupported)
	}
	selectedScheme, authReason := selectInstanceAuthScheme(caps, inst)
	if authReason != "" {
		return r.finishFailed(ctx, compiled.Kind, start, eval, authReason)
	}
	eval.Auth = buildInitialAuthProvenance(inst, selectedScheme)
	if !r.isNamedSecretAllowedForAdapter(adapter, inst) {
		return r.finishFailed(ctx, compiled.Kind, start, eval, provider.FailAuth)
	}
	if projReason := checkInstanceProjectContext(inst, opts.ProjectID); projReason != "" {
		return r.finishFailed(ctx, compiled.Kind, start, eval, projReason)
	}
	if requiresPinnedDefaultEndpoint(inst, selectedScheme) {
		if _, ok := provider.MatchesAdapterDefaultEndpoint(adapter, inst.Endpoint); !ok {
			return r.finishFailed(ctx, compiled.Kind, start, eval, provider.FailAuth)
		}
	}

	// Step 5: Tighten runtime-owned deadline with per-instance CallTimeout ceiling (§7.1).
	attemptOwnDur := r.effectiveOwnDuration(compiled.Timeout, opts, inst.Limits.CallTimeout)
	ownDeadline = start.Add(attemptOwnDur)
	attemptCtx, attemptCancel := context.WithDeadlineCause(runCtx, ownDeadline, errRuntimeDeadline)
	defer attemptCancel()

	if runtimeTimedOut || attemptCtx.Err() != nil || ctx.Err() != nil {
		cause := latchCause(context.Cause(attemptCtx))
		return r.finishFailed(ctx, compiled.Kind, start, eval, classifyContextCause(ctx.Err(), cause))
	}

	// Step 5b: Acquire per-replica admission permits (per-project semaphore,
	// per-instance MaxInFlight semaphore, and per-instance RatePerSec token
	// bucket), waiting at most min(admissionWait, attemptDeadline) before
	// failing fast with FailCapacity and 0 provider requests (runtime-design §7.2).
	admissionStart := time.Now().UTC()
	releaseAdmission, admitted := r.admission.acquire(attemptCtx, strings.TrimSpace(opts.ProjectID), inst)
	if !admitted {
		admissionDur := time.Since(admissionStart)
		reason := provider.FailCapacity
		if errors.Is(ctx.Err(), context.Canceled) {
			reason = provider.FailCanceled
		}
		eval.Attempts = []Attempt{{
			Instance:         inst.Name,
			InstanceRevision: inst.Revision,
			Adapter:          adapter.Name(),
			AdapterVersion:   adapter.Version(),
			ConfiguredModel:  inst.Model,
			Location:         inst.Location,
			Started:          admissionStart,
			Duration:         admissionDur,
			Requests:         0,
			Retries:          0,
			Outcome:          AttemptFailed,
			Reason:           string(reason),
		}}
		return r.finishFailed(ctx, compiled.Kind, start, eval, reason)
	}

	// Step 6: Build credential-free ModelRequest from compiled spec + selected fields only (AC-2).
	req := buildModelRequest(evalID, compiled, selectedFields, selectedChoiceSets)
	instView := inst.View()
	transport := r.transportForInstance(inst, selectedScheme)
	callTracker := &evalCallTracker{}
	callCtx := WithEvalProjectID(context.WithValue(attemptCtx, evalCallTrackerKey{}, callTracker), opts.ProjectID)

	attemptStart := time.Now().UTC()
	res, invokeErr, panicked := invokeSafely(callCtx, adapter, transport, instView, req)
	releaseAdmission()
	attemptDuration := time.Since(attemptStart)
	callerErr := ctx.Err()
	attemptErr := attemptCtx.Err()
	attemptCause := latchCause(context.Cause(attemptCtx))
	attemptCancel()

	var cleanResolvedModel string
	var resolvedModelViolated bool
	if !panicked && res.ResolvedModel != "" {
		if m, ok := sanitizeProviderIdentifier(res.ResolvedModel); ok {
			cleanResolvedModel = m
		} else {
			resolvedModelViolated = true
		}
	}
	var cleanRequestID string
	if !panicked && res.ProviderRequestID != "" {
		if id, ok := sanitizeProviderIdentifier(res.ProviderRequestID); ok {
			cleanRequestID = id
		}
	}

	attemptRequests := 1
	if callTracker.preCallAuthFailed.Load() || isPreCallEgressError(invokeErr) {
		attemptRequests = 0
	}
	if credType, saEmail := callTracker.authIdentity(); credType != "" || saEmail != "" {
		if eval.Auth == nil {
			eval.Auth = &AuthProvenance{AuthKind: inst.Auth.Kind}
		}
		if credType != "" {
			eval.Auth.CredentialType = credType
		}
		if saEmail != "" {
			eval.Auth.ServiceAccountEmail = saEmail
		}
	}
	if secName, scopeUsed, secVer := callTracker.secretRefProvenance(); secName != "" || scopeUsed != "" || secVer > 0 {
		secScope, _ := provider.SecretRefModeFromOptions(inst.Options)
		if eval.Auth == nil {
			eval.Auth = &AuthProvenance{AuthKind: inst.Auth.Kind}
		}
		if eval.Auth.SecretRef == nil {
			eval.Auth.SecretRef = &SecretRefProvenance{
				Name:  secName,
				Scope: secScope,
			}
		}
		if secName != "" {
			eval.Auth.SecretRef.Name = secName
		}
		if scopeUsed != "" {
			eval.Auth.SecretRef.ScopeUsed = scopeUsed
		}
		if secVer > 0 {
			eval.Auth.SecretVersion = secVer
		}
	}

	attempt := Attempt{
		Instance:          inst.Name,
		InstanceRevision:  inst.Revision,
		Adapter:           adapter.Name(),
		AdapterVersion:    adapter.Version(),
		ConfiguredModel:   inst.Model,
		ReportedModel:     cleanResolvedModel,
		ProviderRequestID: cleanRequestID,
		Location:          inst.Location,
		Started:           attemptStart,
		Duration:          attemptDuration,
		Requests:          attemptRequests,
		Retries:           0,
		HTTPStatusClass:   provider.HTTPStatusClass(res.HTTPStatus),
	}

	if panicked {
		attempt.Outcome = AttemptFailed
		attempt.Reason = string(provider.FailInternal)
		eval.Attempts = []Attempt{attempt}
		return r.finishFailed(ctx, compiled.Kind, start, eval, provider.FailInternal)
	}

	if invokeErr != nil {
		reason, statusClass := classifyInvokeError(invokeErr, callerErr, attemptErr, attemptCause)
		if statusClass != "" {
			attempt.HTTPStatusClass = statusClass
		}
		if pe, ok := provider.AsProviderError(invokeErr); ok && provider.IsValidAuthHint(pe.AuthHint) {
			if eval.Auth == nil {
				eval.Auth = &AuthProvenance{AuthKind: inst.Auth.Kind}
			}
			eval.Auth.ReasonHint = pe.AuthHint
		}
		attempt.Outcome = AttemptFailed
		attempt.Reason = string(reason)
		eval.Attempts = []Attempt{attempt}
		if cleanResolvedModel != "" {
			eval.ResolvedModel = cleanResolvedModel
		}
		eval.Notes = mergeNotes(eval.Notes, res.Notes)
		if resolvedModelViolated {
			eval.Notes = mergeNotes(eval.Notes, []string{provider.NoteModelUnreported})
		}
		return r.finishFailed(ctx, compiled.Kind, start, eval, reason)
	}

	// Even if Adapter.Invoke returned nil error, enforce deadline/cancel if the
	// context expired concurrently before Invoke returned.
	if runtimeTimedOut || callerErr != nil || attemptErr != nil {
		reason := classifyContextCause(callerErr, attemptCause)
		attempt.Outcome = AttemptFailed
		attempt.Reason = string(reason)
		eval.Attempts = []Attempt{attempt}
		if cleanResolvedModel != "" {
			eval.ResolvedModel = cleanResolvedModel
		}
		eval.Notes = mergeNotes(eval.Notes, res.Notes)
		if resolvedModelViolated {
			eval.Notes = mergeNotes(eval.Notes, []string{provider.NoteModelUnreported})
		}
		return r.finishFailed(ctx, compiled.Kind, start, eval, reason)
	}
	eval.ResolvedModel = cleanResolvedModel
	eval.Notes = mergeNotes(eval.Notes, res.Notes)
	if cleanResolvedModel == "" {
		eval.Notes = mergeNotes(eval.Notes, []string{provider.NoteModelUnreported})
	}
	if !eval.HasUsageUnreported() && hasMeasuredProviderUsage(res.Usage) {
		eval.Usage = cloneProviderUsage(res.Usage)
	}

	// Step 7: Validate decoded answers, compute portable metrics, apply argmax/abstain/tie,
	// classify outcome, and project to consumer Kind (runtime-design §§5, 6, 8.1; AC-6).
	dec, class, reason := classifyAndProject(compiled, caps, res, eval)
	eval.Class = class
	eval.Reason = reason

	switch class {
	case ClassSuccess:
		attempt.Outcome = AttemptSuccess
		eval.Attempts = []Attempt{attempt}
		eval.Elapsed = time.Since(start)
		dec.Eval = eval
		r.logEvaluation(ctx, eval)
		return dec, nil

	case ClassUncertain:
		attempt.Outcome = AttemptUncertain
		attempt.Reason = reason
		eval.Attempts = []Attempt{attempt}
		eval.Elapsed = time.Since(start)
		r.logEvaluation(ctx, eval)
		return Decision{Kind: compiled.Kind, Eval: eval}, &EvaluationError{Eval: eval}

	default:
		attempt.Outcome = AttemptFailed
		attempt.Reason = reason
		eval.Attempts = []Attempt{attempt}
		eval.Elapsed = time.Since(start)
		r.logEvaluation(ctx, eval)
		return Decision{Kind: compiled.Kind, Eval: eval}, &EvaluationError{Eval: eval}
	}
}

// ListModels invokes the optional provider.ModelLister SPI extension for inst
// using the Runtime's authenticated SSRF-safe egress transport and effective
// deadline (P1 brief §2, §3; provider-design §7.4).
func (r *Runtime) ListModels(ctx context.Context, inst Instance) ([]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !inst.Enabled {
		return nil, provider.NewProviderError(provider.FailNoEligibleInstance, false, 0, 0, "instance is disabled")
	}
	if projID := strings.TrimSpace(EvalProjectIDFromContext(ctx)); projID != "" && r.projectAllowlist != nil {
		allowed, err := r.projectAllowlist.AllowedInstances(ctx, projID)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, provider.NewProviderError(provider.FailUnavailable, true, 0, 0, "failed to resolve project instance allowlist")
		}
		if !slices.Contains(allowed, inst.Name) {
			return nil, provider.NewProviderError(provider.FailNoEligibleInstance, false, 0, 0, "instance is not allowed for project")
		}
	}
	adapter, ok := r.registry.Get(inst.Adapter)
	if !ok {
		return nil, provider.NewProviderError(provider.FailNoEligibleInstance, false, 0, 0, "adapter is not registered")
	}
	lister, ok := adapter.(provider.ModelLister)
	if !ok {
		return nil, provider.NewProviderError(provider.FailUnsupported, false, 0, 0, "adapter does not implement ModelLister")
	}
	caps, err, capsPanicked := capabilitiesSafely(adapter, inst.View())
	if capsPanicked {
		return nil, provider.NewProviderError(provider.FailInternal, false, 0, 0, "adapter panic recovered")
	}
	if err != nil {
		if pe, ok := provider.AsProviderError(err); ok {
			return nil, pe
		}
		return nil, provider.NewProviderError(provider.FailInternal, false, 0, 0, "failed to inspect adapter capabilities")
	}
	selectedScheme, authReason := selectInstanceAuthScheme(caps, inst)
	if authReason != "" {
		return nil, provider.NewProviderError(authReason, false, 0, 0, "instance auth scheme is not supported")
	}
	if !r.isNamedSecretAllowedForAdapter(adapter, inst) {
		return nil, provider.NewProviderError(provider.FailAuth, false, 0, 0, "secret reference name is not permitted for adapter")
	}
	if projReason := checkInstanceProjectContext(inst, EvalProjectIDFromContext(ctx)); projReason != "" {
		return nil, provider.NewProviderError(projReason, false, 0, 0, "project context is required for project-scoped secret reference")
	}
	if requiresPinnedDefaultEndpoint(inst, selectedScheme) {
		if _, ok := provider.MatchesAdapterDefaultEndpoint(adapter, inst.Endpoint); !ok {
			return nil, provider.NewProviderError(provider.FailAuth, false, 0, 0, "custom endpoint is not permitted for named-secret or google_access_token instances")
		}
	}

	start := time.Now()
	ownDur := r.maxEvalTime
	callTimeout := DefaultCallTimeout
	if inst.Limits.CallTimeout > 0 {
		callTimeout = clampDuration(inst.Limits.CallTimeout, MaxAllowedEvalTime)
	}
	if callTimeout < ownDur {
		ownDur = callTimeout
	}
	ownDeadline := start.Add(ownDur)

	callCtx, cancel := context.WithDeadlineCause(ctx, ownDeadline, errRuntimeDeadline)
	defer cancel()

	models, err, listPanicked := listModelsSafely(callCtx, lister, r.transportForInstance(inst, selectedScheme), inst.View())
	if listPanicked {
		return nil, provider.NewProviderError(provider.FailInternal, false, 0, 0, "adapter panic recovered")
	}
	if err != nil {
		reason, _ := classifyInvokeError(err, ctx.Err(), callCtx.Err(), context.Cause(callCtx))
		httpStatus := 0
		retryable := false
		var retryAfter time.Duration
		msg := ""
		if pe, ok := provider.AsProviderError(err); ok {
			httpStatus = pe.HTTPStatus
			retryable = pe.Retryable
			retryAfter = pe.RetryAfter
			msg = pe.Message
		}
		return nil, provider.NewProviderError(reason, retryable, retryAfter, httpStatus, msg)
	}
	clean := make([]string, 0, min(len(models), MaxListedModels))
	for _, m := range models {
		if s, ok := sanitizeProviderIdentifier(m); ok {
			clean = append(clean, s)
			if len(clean) == MaxListedModels {
				break
			}
		}
	}
	return clean, nil
}

func (r *Runtime) effectiveOwnDuration(ruleTimeout time.Duration, opts EvalOptions, callTimeout time.Duration) time.Duration {
	effMax := r.maxEvalTime
	if opts.MaxEvalTime > 0 {
		clamped := clampDuration(opts.MaxEvalTime, MaxAllowedEvalTime)
		if clamped < effMax {
			effMax = clamped
		}
	}

	effRule := DefaultRuleTimeout
	if ruleTimeout > 0 {
		effRule = ruleTimeout
	}

	d := effRule
	if effMax < d {
		d = effMax
	}

	effCall := DefaultCallTimeout
	if callTimeout > 0 {
		effCall = clampDuration(callTimeout, MaxAllowedEvalTime)
	}
	if effCall < d {
		d = effCall
	}

	if opts.Budget > 0 && opts.Budget < d {
		d = opts.Budget
	}
	if opts.EngineMaxEvalTime > 0 {
		margin := max(opts.EngineDeadlineMargin, 0)
		engDur := max(opts.EngineMaxEvalTime-margin, 0)
		if engDur < d {
			d = engDur
		}
	}
	return d
}

func (r *Runtime) resolveBinding(
	resolveCtx context.Context,
	callerCtx context.Context,
	compiled *spec.Compiled,
	opts EvalOptions,
) (Instance, provider.Adapter, provider.Capabilities, provider.FailReason) {
	// Pools land in P4 (runtime-design §13).
	if compiled.Spec.Binding.Pool != "" {
		return Instance{}, nil, provider.Capabilities{}, provider.FailNoEligibleInstance
	}

	// Per-project instance allowlist gate (D1; runtime-design §4.5 rule 4).
	// Checked before any credential resolution, admission wait, or provider I/O.
	projectID := strings.TrimSpace(opts.ProjectID)
	var allowedSet map[string]struct{}
	checkAllowlist := false
	if r.projectAllowlist != nil {
		if projectID == "" {
			if opts.RequireProject {
				return Instance{}, nil, provider.Capabilities{}, provider.FailNoEligibleInstance
			}
		} else {
			checkAllowlist = true
			allowed, err := r.projectAllowlist.AllowedInstances(resolveCtx, projectID)
			if err != nil {
				if callerCtx.Err() != nil || resolveCtx.Err() != nil {
					return Instance{}, nil, provider.Capabilities{}, classifyContextCause(callerCtx.Err(), context.Cause(resolveCtx))
				}
				return Instance{}, nil, provider.Capabilities{}, provider.FailUnavailable
			}
			allowedSet = make(map[string]struct{}, len(allowed))
			for _, name := range allowed {
				allowedSet[name] = struct{}{}
			}
		}
	} else if opts.RequireProject && projectID == "" {
		return Instance{}, nil, provider.Capabilities{}, provider.FailNoEligibleInstance
	}

	var inst Instance
	switch {
	case opts.Instance != nil:
		if compiled.Spec.Binding.Instance != "" && opts.Instance.Name != compiled.Spec.Binding.Instance {
			return Instance{}, nil, provider.Capabilities{}, provider.FailNoEligibleInstance
		}
		inst = cloneInstance(*opts.Instance)

	case compiled.Spec.Binding.Instance != "":
		if checkAllowlist {
			if _, ok := allowedSet[compiled.Spec.Binding.Instance]; !ok {
				return Instance{}, nil, provider.Capabilities{}, provider.FailNoEligibleInstance
			}
		}
		if r.instances == nil {
			return Instance{}, nil, provider.Capabilities{}, provider.FailNoEligibleInstance
		}
		got, found, err := r.instances.GetInstance(resolveCtx, compiled.Spec.Binding.Instance)
		if err != nil {
			if callerCtx.Err() != nil || resolveCtx.Err() != nil {
				return Instance{}, nil, provider.Capabilities{}, classifyContextCause(callerCtx.Err(), context.Cause(resolveCtx))
			}
			return Instance{}, nil, provider.Capabilities{}, provider.FailUnavailable
		}
		if !found {
			return Instance{}, nil, provider.Capabilities{}, provider.FailNoEligibleInstance
		}
		inst = cloneInstance(got)

	case r.defaultInstance != "":
		if checkAllowlist {
			if _, ok := allowedSet[r.defaultInstance]; !ok {
				return Instance{}, nil, provider.Capabilities{}, provider.FailNoEligibleInstance
			}
		}
		if r.instances == nil {
			return Instance{}, nil, provider.Capabilities{}, provider.FailNoEligibleInstance
		}
		got, found, err := r.instances.GetInstance(resolveCtx, r.defaultInstance)
		if err != nil {
			if callerCtx.Err() != nil || resolveCtx.Err() != nil {
				return Instance{}, nil, provider.Capabilities{}, classifyContextCause(callerCtx.Err(), context.Cause(resolveCtx))
			}
			return Instance{}, nil, provider.Capabilities{}, provider.FailUnavailable
		}
		if !found {
			return Instance{}, nil, provider.Capabilities{}, provider.FailNoEligibleInstance
		}
		inst = cloneInstance(got)

	default:
		if r.instances == nil {
			return Instance{}, nil, provider.Capabilities{}, provider.FailNoEligibleInstance
		}
		all, err := r.instances.ListInstances(resolveCtx)
		if err != nil {
			if callerCtx.Err() != nil || resolveCtx.Err() != nil {
				return Instance{}, nil, provider.Capabilities{}, classifyContextCause(callerCtx.Err(), context.Cause(resolveCtx))
			}
			return Instance{}, nil, provider.Capabilities{}, provider.FailUnavailable
		}
		var enabled []Instance
		for _, candidate := range all {
			if !candidate.Enabled {
				continue
			}
			if checkAllowlist {
				if _, ok := allowedSet[candidate.Name]; !ok {
					continue
				}
			}
			enabled = append(enabled, candidate)
		}
		if len(enabled) != 1 {
			return Instance{}, nil, provider.Capabilities{}, provider.FailNoEligibleInstance
		}
		inst = cloneInstance(enabled[0])
	}

	if checkAllowlist {
		if _, ok := allowedSet[inst.Name]; !ok {
			return Instance{}, nil, provider.Capabilities{}, provider.FailNoEligibleInstance
		}
	}

	if !inst.Enabled {
		return Instance{}, nil, provider.Capabilities{}, provider.FailNoEligibleInstance
	}
	adapter, ok := r.registry.Get(inst.Adapter)
	if !ok {
		return Instance{}, nil, provider.Capabilities{}, provider.FailNoEligibleInstance
	}
	view := inst.View()
	caps, err := adapter.Capabilities(view)
	if err != nil {
		if pe, ok := provider.AsProviderError(err); ok && pe.Reason.Valid() {
			return Instance{}, nil, provider.Capabilities{}, pe.Reason
		}
		return Instance{}, nil, provider.Capabilities{}, provider.FailNoEligibleInstance
	}
	if !caps.SideEffectFree {
		return Instance{}, nil, provider.Capabilities{}, provider.FailNoEligibleInstance
	}

	spCopy := cloneSpec(compiled.Spec)
	if err := spec.ValidatePortable(&spCopy, caps.SpecLimits()); err != nil {
		return Instance{}, nil, provider.Capabilities{}, provider.FailNoEligibleInstance
	}
	if err := adapter.Validate(view, spCopy); err != nil {
		return Instance{}, nil, provider.Capabilities{}, provider.FailNoEligibleInstance
	}

	return inst, adapter, caps, ""
}

func canonicalScopesKey(scopes []string) string {
	if len(scopes) == 0 {
		return ""
	}
	var b strings.Builder
	for _, s := range scopes {
		b.WriteString(strconv.Itoa(len(s)))
		b.WriteByte(':')
		b.WriteString(s)
		b.WriteByte(';')
	}
	return b.String()
}

func placementFromAuthScheme(scheme provider.AuthScheme, instOpts map[string]string) egress.Placement {
	if scheme.Kind == provider.AuthKindGoogleAccessToken {
		return egress.ADCBearerPlacement(instOpts[provider.OptionQuotaProject])
	}
	return egress.Placement{
		Kind:       scheme.Kind,
		HeaderName: scheme.HeaderName,
	}
}

func canonicalPlacementKey(p egress.Placement) (provider.AuthKind, string) {
	kind := p.Kind
	if kind == "" {
		kind = provider.AuthKindBearer
	}
	var hdr string
	if p.HeaderName != "" {
		hdr = textproto.CanonicalMIMEHeaderKey(p.HeaderName)
	}
	return kind, hdr
}

func (r *Runtime) transportForInstance(inst Instance, adapterScheme provider.AuthScheme) provider.Transport {
	placement := placementFromAuthScheme(adapterScheme, inst.Options)
	placementKind, placementHeader := canonicalPlacementKey(placement)
	effScopes := inst.Auth.Scopes
	if adapterScheme.Kind == provider.AuthKindGoogleAccessToken && len(effScopes) == 0 && len(adapterScheme.Scopes) > 0 {
		effScopes = adapterScheme.Scopes
	}
	secScope, isNamedRef := provider.SecretRefModeFromOptions(inst.Options)
	key := transportKey{
		id:               inst.ID,
		name:             inst.Name,
		revision:         inst.Revision,
		adapter:          inst.Adapter,
		endpoint:         inst.Endpoint,
		authKind:         inst.Auth.Kind,
		secretName:       inst.Auth.SecretName,
		isNamedSecretRef: isNamedRef,
		secretScope:      secScope,
		headerName:       inst.Auth.HeaderName,
		audience:         inst.Auth.Audience,
		scopes:           canonicalScopesKey(effScopes),
		quotaProject:     placement.QuotaProject,
		placementKind:    placementKind,
		placementHeader:  placementHeader,
	}
	r.transportMu.Lock()
	defer r.transportMu.Unlock()

	if tr, ok := r.transports[key]; ok {
		return tr
	}
	// Evict any superseded revision for the same instance so idle connection
	// pools do not accumulate across config updates.
	for existingKey, existingTr := range r.transports {
		sameID := inst.ID != "" && existingKey.id == inst.ID
		sameName := inst.ID == "" && existingKey.name == inst.Name
		if (sameID || sameName) && existingKey.revision != inst.Revision {
			existingTr.CloseIdleConnections()
			delete(r.transports, existingKey)
		}
	}

	instSnap := cloneInstance(inst)
	if len(effScopes) > 0 {
		instSnap.Auth.Scopes = slices.Clone(effScopes)
	}
	creds := r.credentials
	tokSrc := r.tokenSource
	bearer := func(reqCtx context.Context) (string, error) {
		src := creds
		if instSnap.Auth.Kind == string(provider.AuthKindGoogleAccessToken) {
			src = tokSrc
		}
		if src == nil {
			markPreCallAuthFailed(reqCtx)
			return "", egress.ErrAuth
		}
		tok, err := src(reqCtx, instSnap)
		if err != nil || strings.TrimSpace(tok) == "" {
			markPreCallAuthFailed(reqCtx)
		}
		return tok, err
	}
	opts := make([]egress.Option, 0, len(r.egressOpts)+1)
	opts = append(opts, egress.WithCredentialPlacement(placement))
	opts = append(opts, r.egressOpts...)
	tr := egress.NewClient(bearer, opts...)
	r.transports[key] = tr
	return tr
}

func (r *Runtime) finishFailed(
	ctx context.Context,
	kind spec.Kind,
	start time.Time,
	eval *Evaluation,
	reason provider.FailReason,
) (Decision, error) {
	if !reason.Valid() {
		reason = provider.FailInternal
	}
	eval.Class = ClassFailed
	eval.Reason = string(reason)
	eval.Elapsed = time.Since(start)
	r.logEvaluation(ctx, eval)
	return Decision{Kind: kind, Eval: eval}, &EvaluationError{Eval: eval}
}

func (r *Runtime) logEvaluation(ctx context.Context, eval *Evaluation) {
	if r.logger == nil || eval == nil {
		return
	}
	var instName, adapterName string
	if len(eval.Attempts) > 0 {
		instName = eval.Attempts[len(eval.Attempts)-1].Instance
		adapterName = eval.Attempts[len(eval.Attempts)-1].Adapter
	}
	// Never log input field values, Input.SHA256, provider response bodies, or credentials (runtime-design §§7.6, 10).
	r.logger.DebugContext(ctx, "system_one evaluation completed",
		slog.String("eval_id", eval.EvalID),
		slog.String("class", string(eval.Class)),
		slog.String("reason", eval.Reason),
		slog.String("instance", instName),
		slog.String("adapter", adapterName),
		slog.String("configured_model", eval.ConfiguredModel),
		slog.String("resolved_model", eval.ResolvedModel),
		slog.Int64("elapsed_ms", eval.Elapsed.Milliseconds()),
	)
}

func selectInputFields(
	compiled *spec.Compiled,
	input Input,
) ([]provider.InputField, map[string][]string, InputFingerprint, int, provider.FailReason) {
	if err := spec.ValidateInputs(compiled.Inputs, compiled.Environment); err != nil {
		return nil, nil, InputFingerprint{}, 0, provider.FailInvalidRequest
	}

	byName := make(map[string]provider.InputField, len(input.Fields))
	for _, f := range input.Fields {
		if _, dup := byName[f.Name]; dup {
			return nil, nil, InputFingerprint{}, 0, provider.FailInvalidRequest
		}
		byName[f.Name] = f
	}

	// Validate dynamic choice sets declared in compiled.Environment (runtime-design §4.4).
	var selectedChoiceSets map[string][]string
	choiceSetBytes := 0
	choiceSetOversize := false
	if len(input.ChoiceSets) > 0 {
		setNames := make([]string, 0, len(input.ChoiceSets))
		for k := range input.ChoiceSets {
			setNames = append(setNames, k)
		}
		slices.Sort(setNames)
		selectedChoiceSets = make(map[string][]string, len(setNames))
		for _, setName := range setNames {
			decl, ok := compiled.Environment.ChoiceSet(setName)
			if !ok {
				return nil, nil, InputFingerprint{}, 0, provider.FailInvalidRequest
			}
			labels := input.ChoiceSets[setName]
			if len(labels) > decl.MaxItems {
				choiceSetOversize = true
			}
			seenLabels := make(map[string]struct{}, len(labels))
			for _, lbl := range labels {
				if lbl == "" || !utf8.ValidString(lbl) {
					return nil, nil, InputFingerprint{}, 0, provider.FailInvalidRequest
				}
				if _, dup := seenLabels[lbl]; dup {
					return nil, nil, InputFingerprint{}, 0, provider.FailInvalidRequest
				}
				seenLabels[lbl] = struct{}{}
				if len(lbl) > decl.MaxLabelBytes {
					choiceSetOversize = true
				}
				choiceSetBytes += len(lbl)
			}
			selectedChoiceSets[setName] = slices.Clone(labels)
		}
	}

	truncate := compiled.OnOversize == spec.OversizeTruncate
	selected := make([]provider.InputField, 0, len(compiled.Inputs))
	fieldOversize := false

	for _, name := range compiled.Inputs {
		decl, ok := compiled.Environment.Field(name)
		if !ok {
			return nil, nil, InputFingerprint{}, 0, provider.FailInvalidRequest
		}
		supplied, present := byName[name]
		if !present {
			return nil, nil, InputFingerprint{}, 0, provider.FailInvalidRequest
		}
		if supplied.Kind != "" && supplied.Kind != decl.Kind {
			return nil, nil, InputFingerprint{}, 0, provider.FailInvalidRequest
		}
		if !utf8.ValidString(supplied.Value) {
			return nil, nil, InputFingerprint{}, 0, provider.FailInvalidRequest
		}
		if decl.Kind == spec.FieldKindJSON && !json.Valid([]byte(supplied.Value)) {
			return nil, nil, InputFingerprint{}, 0, provider.FailInvalidRequest
		}

		val := supplied.Value
		truncated := false
		if decl.MaxBytes > 0 && len(val) > decl.MaxBytes {
			if truncate {
				val, truncated = truncateUTF8(val, decl.MaxBytes)
			} else {
				fieldOversize = true
			}
		}
		selected = append(selected, provider.InputField{
			Name:      name,
			Kind:      decl.Kind,
			Value:     val,
			Truncated: truncated,
			Trust:     decl.Trust,
		})
	}

	// Choice set labels are identifiers and are never truncated.
	if choiceSetOversize {
		fp := buildInputFingerprint(selected, input.Params, selectedChoiceSets)
		return nil, nil, fp, sumFieldBytes(selected) + choiceSetBytes, provider.FailInputTooLarge
	}

	totalBytes := sumFieldBytes(selected) + choiceSetBytes
	if compiled.Environment.MaxTotalBytes > 0 && totalBytes > compiled.Environment.MaxTotalBytes {
		if truncate {
			if choiceSetBytes > compiled.Environment.MaxTotalBytes {
				fp := buildInputFingerprint(selected, input.Params, selectedChoiceSets)
				return nil, nil, fp, totalBytes, provider.FailInputTooLarge
			}
			remaining := compiled.Environment.MaxTotalBytes - choiceSetBytes
			for i := range selected {
				if len(selected[i].Value) <= remaining {
					remaining -= len(selected[i].Value)
					continue
				}
				cutVal, didCut := truncateUTF8(selected[i].Value, remaining)
				selected[i].Value = cutVal
				if didCut {
					selected[i].Truncated = true
				}
				remaining -= len(cutVal)
			}
			totalBytes = sumFieldBytes(selected) + choiceSetBytes
		} else {
			fieldOversize = true
		}
	}

	fp := buildInputFingerprint(selected, input.Params, selectedChoiceSets)
	if fieldOversize {
		return nil, nil, fp, totalBytes, provider.FailInputTooLarge
	}
	return selected, selectedChoiceSets, fp, totalBytes, ""
}

func checkChoiceSetInstanceLimits(choiceSets map[string][]string, caps provider.Capabilities) provider.FailReason {
	if len(choiceSets) == 0 {
		return ""
	}
	lim, ok := caps.Primitives[spec.PrimitiveChoice]
	if !ok || !lim.Supported {
		return provider.FailUnsupported
	}
	for _, labels := range choiceSets {
		if lim.MaxOptions > 0 && len(labels) > lim.MaxOptions {
			return provider.FailUnsupported
		}
	}
	return ""
}

// truncateUTF8 cuts s to at most maxBytes bytes without splitting a multi-byte
// UTF-8 rune. Returns the truncated string and whether any bytes were removed.
func truncateUTF8(s string, maxBytes int) (string, bool) {
	if maxBytes <= 0 {
		return "", len(s) > 0
	}
	if len(s) <= maxBytes {
		return s, false
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

func sumFieldBytes(fields []provider.InputField) int {
	total := 0
	for _, f := range fields {
		total += len(f.Value)
	}
	return total
}

func buildInputFingerprint(fields []provider.InputField, params map[string]string, choiceSets map[string][]string) InputFingerprint {
	fps := make([]FieldFingerprint, len(fields))
	for i, f := range fields {
		fps[i] = FieldFingerprint{
			Name:      f.Name,
			Bytes:     len(f.Value),
			Truncated: f.Truncated,
		}
	}
	return InputFingerprint{
		Fields: fps,
		SHA256: computeSelectedInputSHA256(fields, params, choiceSets),
	}
}

func checkMaxInputTokensBytes(totalBytes int, caps provider.Capabilities, limits InstanceLimits) provider.FailReason {
	effMaxTokens := caps.MaxInputTokens
	if limits.MaxInputTokens > 0 && (effMaxTokens <= 0 || limits.MaxInputTokens < effMaxTokens) {
		effMaxTokens = limits.MaxInputTokens
	}

	if caps.Truncation == provider.TruncationSilent {
		// Silent-truncation adapters must prove non-truncation conservatively (1 byte/token).
		if effMaxTokens <= 0 || totalBytes > effMaxTokens*SilentBytesPerTokenPrecheck {
			return provider.FailInputTooLarge
		}
		return ""
	}

	if effMaxTokens > 0 && totalBytes > effMaxTokens*BytesPerTokenPrecheck {
		return provider.FailInputTooLarge
	}
	return ""
}

func selectInstanceAuthScheme(caps provider.Capabilities, inst Instance) (provider.AuthScheme, provider.FailReason) {
	if len(caps.SupportedAuth) > 0 {
		kind := inst.Auth.Kind
		if kind == "" {
			if _, hasEmpty := caps.SupportedAuth[""]; !hasEmpty {
				if caps.AuthScheme.Kind == provider.AuthKindNone {
					kind = string(provider.AuthKindNone)
				} else {
					kind = provider.InstanceAuthAPIKey
				}
			}
		}
		scheme, ok := caps.SupportedAuth[kind]
		if !ok {
			return provider.AuthScheme{}, provider.FailUnsupported
		}
		if fail := checkInstanceAuthSupport(scheme, inst); fail != "" {
			return provider.AuthScheme{}, fail
		}
		return scheme, ""
	}

	if fail := checkInstanceAuthSupport(caps.AuthScheme, inst); fail != "" {
		return provider.AuthScheme{}, fail
	}
	return caps.AuthScheme, ""
}

func checkInstanceAuthSupport(adapterScheme provider.AuthScheme, inst Instance) provider.FailReason {
	instAuth := inst.Auth
	secScope, isNamedRef := provider.SecretRefModeFromOptions(inst.Options)
	switch adapterScheme.Kind {
	case provider.AuthKindBearer:
		switch instAuth.Kind {
		case "", provider.InstanceAuthAPIKey, string(provider.AuthKindBearer):
			if isNamedRef {
				if !provider.IsValidSecretRefScope(secScope) || !provider.IsValidSecretRefName(instAuth.SecretName) {
					return provider.FailInvalidRequest
				}
			}
			if err := egress.ValidatePlacement(placementFromAuthScheme(adapterScheme, inst.Options)); err != nil {
				return provider.FailInternal
			}
			return ""
		default:
			return provider.FailUnsupported
		}
	case provider.AuthKindHeader:
		switch instAuth.Kind {
		case "", provider.InstanceAuthAPIKey:
			if isNamedRef {
				if !provider.IsValidSecretRefScope(secScope) || !provider.IsValidSecretRefName(instAuth.SecretName) {
					return provider.FailInvalidRequest
				}
			}
			if err := egress.ValidatePlacement(placementFromAuthScheme(adapterScheme, inst.Options)); err != nil {
				return provider.FailInternal
			}
			return ""
		default:
			return provider.FailUnsupported
		}
	case provider.AuthKindGoogleAccessToken:
		if instAuth.Kind != string(provider.AuthKindGoogleAccessToken) {
			return provider.FailUnsupported
		}
		if isNamedRef {
			return provider.FailInvalidRequest
		}
		effScopes := instAuth.Scopes
		if len(effScopes) == 0 && len(adapterScheme.Scopes) > 0 {
			effScopes = adapterScheme.Scopes
		}
		if !hasValidGoogleOAuthScopes(effScopes) {
			return provider.FailInvalidRequest
		}
		if qp := inst.Options[provider.OptionQuotaProject]; qp != "" {
			if err := egress.ValidateQuotaProject(qp); err != nil {
				return provider.FailInvalidRequest
			}
		}
		if err := egress.ValidatePlacement(placementFromAuthScheme(adapterScheme, inst.Options)); err != nil {
			return provider.FailInternal
		}
		return ""
	case provider.AuthKindNone:
		if instAuth.Kind == "" || instAuth.Kind == string(provider.AuthKindNone) {
			if isNamedRef {
				return provider.FailInvalidRequest
			}
			if err := egress.ValidatePlacement(placementFromAuthScheme(adapterScheme, inst.Options)); err != nil {
				return provider.FailInternal
			}
			return ""
		}
		return provider.FailUnsupported
	default:
		return provider.FailUnsupported
	}
}

func checkInstanceProjectContext(inst Instance, projectID string) provider.FailReason {
	secScope, isNamedRef := provider.SecretRefModeFromOptions(inst.Options)
	if !isNamedRef {
		return ""
	}
	switch secScope {
	case provider.SecretScopeProject, provider.SecretScopeProjectOrHub:
		if strings.TrimSpace(projectID) == "" {
			return provider.FailInvalidRequest
		}
	}
	return ""
}

func (r *Runtime) isNamedSecretAllowedForAdapter(adapter provider.Adapter, inst Instance) bool {
	_, isNamedRef := provider.SecretRefModeFromOptions(inst.Options)
	if !isNamedRef {
		return true
	}
	return slices.Contains(r.allowedSecretNames(adapter), inst.Auth.SecretName)
}

func requiresPinnedDefaultEndpoint(inst Instance, selectedScheme provider.AuthScheme) bool {
	_, isNamedRef := provider.SecretRefModeFromOptions(inst.Options)
	if isNamedRef {
		return true
	}
	return selectedScheme.Kind == provider.AuthKindGoogleAccessToken ||
		inst.Auth.Kind == string(provider.AuthKindGoogleAccessToken)
}

func isPreCallEgressError(err error) bool {
	if err == nil {
		return false
	}
	return egress.IsAuthError(err) ||
		egress.IsCredentialUnavailable(err) ||
		egress.IsInvalidPlacement(err) ||
		egress.IsSchemeNotAllowed(err) ||
		egress.IsInvalidEndpoint(err) ||
		egress.IsBlockedDestination(err)
}

func buildInitialAuthProvenance(inst Instance, selectedScheme provider.AuthScheme) *AuthProvenance {
	secScope, isNamedRef := provider.SecretRefModeFromOptions(inst.Options)
	if inst.Auth.Kind == "" && !isNamedRef {
		return nil
	}
	authKind := inst.Auth.Kind
	if authKind == "" && isNamedRef {
		authKind = provider.InstanceAuthAPIKey
	}
	prov := &AuthProvenance{
		AuthKind: authKind,
	}
	if isNamedRef {
		prov.SecretRef = &SecretRefProvenance{
			Name:  inst.Auth.SecretName,
			Scope: secScope,
		}
	}
	if selectedScheme.Kind == provider.AuthKindGoogleAccessToken {
		scopes := inst.Auth.Scopes
		if len(scopes) == 0 && len(selectedScheme.Scopes) > 0 {
			scopes = selectedScheme.Scopes
		}
		if len(scopes) > 0 {
			prov.Scopes = slices.Clone(scopes)
		}
		prov.QuotaProject = provider.FormatQuotaProjectProvenance(inst.Options[provider.OptionQuotaProject])
	}
	return prov
}

func hasValidGoogleOAuthScopes(scopes []string) bool {
	if len(scopes) == 0 {
		return false
	}
	for _, s := range scopes {
		if !strings.HasPrefix(s, provider.GoogleScopePrefix) || len(s) <= len(provider.GoogleScopePrefix) {
			return false
		}
		for i := 0; i < len(s); i++ {
			b := s[i]
			if b < 0x21 || b > 0x7e || b == '?' || b == '#' {
				return false
			}
		}
	}
	return true
}

func buildModelRequest(
	evalID string,
	compiled *spec.Compiled,
	selectedFields []provider.InputField,
	selectedChoiceSets map[string][]string,
) provider.ModelRequest {
	spCopy := cloneSpec(compiled.Spec)
	qIDs := make([]string, len(spCopy.Questions))
	for i, q := range spCopy.Questions {
		qIDs[i] = q.ID
	}
	inCopy := make([]provider.InputField, len(selectedFields))
	copy(inCopy, selectedFields)
	var csCopy map[string][]string
	if len(selectedChoiceSets) > 0 {
		csCopy = make(map[string][]string, len(selectedChoiceSets))
		for k, v := range selectedChoiceSets {
			csCopy[k] = slices.Clone(v)
		}
	}
	return provider.ModelRequest{
		Rule:       spCopy,
		Questions:  qIDs,
		Input:      inCopy,
		ChoiceSets: csCopy,
		RequestID:  evalID,
	}
}

func invokeSafely(
	ctx context.Context,
	adapter provider.Adapter,
	transport provider.Transport,
	inst provider.InstanceView,
	req provider.ModelRequest,
) (res provider.ModelResult, err error, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			res = provider.ModelResult{}
			err = nil
		}
	}()
	res, err = adapter.Invoke(ctx, transport, inst, req)
	return res, err, false
}

func capabilitiesSafely(
	adapter provider.Adapter,
	inst provider.InstanceView,
) (caps provider.Capabilities, err error, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			caps = provider.Capabilities{}
			err = nil
		}
	}()
	caps, err = adapter.Capabilities(inst)
	return caps, err, false
}

func listModelsSafely(
	ctx context.Context,
	lister provider.ModelLister,
	transport provider.Transport,
	inst provider.InstanceView,
) (models []string, err error, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			models = nil
			err = nil
		}
	}()
	models, err = lister.ListModels(ctx, transport, inst)
	return models, err, false
}

// classifyContextCause distinguishes runtime-owned deadline expiration
// (FailTimeout) from caller context termination (FailCanceled) by inspecting
// the context cause attached via context.WithDeadlineCause rather than
// comparing wall-clock timestamps (runtime-design §§3, 6.2; Stage 1 §7.0).
func classifyContextCause(callerErr, runCause error) provider.FailReason {
	if errors.Is(runCause, errRuntimeDeadline) {
		return provider.FailTimeout
	}
	if callerErr != nil {
		return provider.FailCanceled
	}
	return provider.FailTimeout
}

func classifyInvokeError(
	err error,
	callerErr error,
	attemptErr error,
	attemptCause error,
) (provider.FailReason, string) {
	var statusClass string
	if pe, ok := provider.AsProviderError(err); ok {
		statusClass = pe.StatusClass()
	}

	// Context termination by cause (§6.2, §7.1, §7.7):
	// context.Cause(attemptCtx) == errRuntimeDeadline -> FailTimeout;
	// otherwise parent ctx.Err() != nil -> FailCanceled.
	if callerErr != nil || attemptErr != nil {
		return classifyContextCause(callerErr, attemptCause), statusClass
	}

	// Egress transport error invariants (AC-4, runtime-design §§4.3, 6.3).
	// Checked via errors.Is BEFORE reading ProviderError.Reason so runtime
	// remains the single authority for egress sentinels whether adapters pass
	// them through raw or wrap them in an error implementing Unwrap.
	// Redirects, blocked destinations, non-https schemes, and malformed/userinfo
	// instance endpoint URLs are all instance-config / egress-policy refusals
	// (FailEgressRefused); oversize responses (>1 MiB) are provider behavior
	// (FailMalformedResponse); missing/malformed tokens map to FailAuth while
	// CredentialSource callback failures map to FailUnavailable.
	switch {
	case egress.IsResponseTooLarge(err):
		return provider.FailMalformedResponse, statusClass
	case egress.IsCredentialUnavailable(err):
		return provider.FailUnavailable, statusClass
	case egress.IsAuthError(err):
		return provider.FailAuth, statusClass
	case egress.IsInvalidPlacement(err):
		return provider.FailInternal, statusClass
	case egress.IsRedirectBlocked(err),
		egress.IsBlockedDestination(err),
		egress.IsSchemeNotAllowed(err),
		egress.IsInvalidEndpoint(err):
		return provider.FailEgressRefused, statusClass
	}

	if pe, ok := provider.AsProviderError(err); ok {
		if pe.Reason.Valid() {
			return pe.Reason, statusClass
		}
		return provider.FailInternal, statusClass
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return provider.FailTimeout, statusClass
	}
	if errors.Is(err, context.Canceled) {
		return provider.FailCanceled, statusClass
	}

	var urlErr *url.Error
	var netErr net.Error
	if errors.As(err, &urlErr) || errors.As(err, &netErr) {
		return provider.FailUnavailable, statusClass
	}

	return provider.FailInternal, statusClass
}

func computeSelectedInputSHA256(
	fields []provider.InputField,
	params map[string]string,
	choiceSets map[string][]string,
) string {
	type fieldKV struct {
		Kind      spec.FieldKind  `json:"kind"`
		Name      string          `json:"name"`
		Truncated bool            `json:"truncated,omitempty"`
		Trust     spec.TrustLabel `json:"trust"`
		Value     string          `json:"value"`
	}
	items := make([]fieldKV, len(fields))
	for i, f := range fields {
		items[i] = fieldKV{
			Kind:      f.Kind,
			Name:      f.Name,
			Truncated: f.Truncated,
			Trust:     f.Trust,
			Value:     f.Value,
		}
	}
	payload := struct {
		ChoiceSets map[string][]string `json:"choice_sets,omitempty"`
		Fields     []fieldKV           `json:"fields"`
		Params     map[string]string   `json:"params,omitempty"`
	}{
		ChoiceSets: choiceSets,
		Fields:     items,
		Params:     params,
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(payload)
	sum := sha256.Sum256(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func newEvalID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "eval-fallback"
	}
	return "eval-" + hex.EncodeToString(b[:])
}

func clampDuration(d, ceiling time.Duration) time.Duration {
	if d > ceiling {
		return ceiling
	}
	return d
}

// sanitizeProviderIdentifier validates a provider-reported identifier string
// before copying it into content-free Evaluation, Attempt, AnswerRecord, or
// debug logs: trimmed, non-empty, at most MaxProviderIdentifierBytes (128
// bytes), and printable non-whitespace ASCII [\x21-\x7e].
func sanitizeProviderIdentifier(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" || len(s) > MaxProviderIdentifierBytes {
		return "", false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return "", false
		}
	}
	return s, true
}

func mergeNotes(existing []string, added []string) []string {
	out := existing
	for _, n := range added {
		if !provider.IsValidNote(n) || slices.Contains(out, n) {
			continue
		}
		out = append(out, n)
	}
	return out
}

func hasMeasuredProviderUsage(u provider.Usage) bool {
	if u.InputTokens < 0 || u.OutputTokens < 0 {
		return false
	}
	return u.InputTokens > 0 || u.OutputTokens > 0 || u.Truncated != nil
}

func cloneProviderUsage(u provider.Usage) *provider.Usage {
	cp := provider.Usage{
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
	}
	if u.Truncated != nil {
		t := *u.Truncated
		cp.Truncated = &t
	}
	return &cp
}

func cloneInstance(inst Instance) Instance {
	cp := inst
	if len(inst.Options) > 0 {
		cp.Options = make(map[string]string, len(inst.Options))
		for k, v := range inst.Options {
			cp.Options[k] = v
		}
	}
	if len(inst.Auth.Scopes) > 0 {
		cp.Auth.Scopes = append([]string(nil), inst.Auth.Scopes...)
	}
	return cp
}

func cloneSpec(s spec.Spec) spec.Spec {
	cp := s
	if len(s.Questions) > 0 {
		cp.Questions = make([]spec.Question, len(s.Questions))
		for i, q := range s.Questions {
			qcp := q
			if q.Criteria != nil {
				crit := *q.Criteria
				qcp.Criteria = &crit
			}
			if len(q.Options) > 0 {
				qcp.Options = append([]spec.Option(nil), q.Options...)
			}
			if len(q.ExtraOptions) > 0 {
				qcp.ExtraOptions = append([]spec.Option(nil), q.ExtraOptions...)
			}
			if len(q.Levels) > 0 {
				qcp.Levels = append([]spec.Level(nil), q.Levels...)
			}
			cp.Questions[i] = qcp
		}
	}
	if s.Required != nil {
		cp.Required = append([]string(nil), s.Required...)
	}
	if s.Output != nil {
		out := *s.Output
		if len(s.Output.Match) > 0 {
			out.Match = append([]string(nil), s.Output.Match...)
		}
		cp.Output = &out
	}
	if s.UncertainIf != nil {
		cp.UncertainIf = append([]spec.UncertainRule(nil), s.UncertainIf...)
	}
	return cp
}
