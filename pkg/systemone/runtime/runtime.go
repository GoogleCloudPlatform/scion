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
	"log/slog"
	"net"
	"net/url"
	"slices"
	"sync"
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
// wiring) must never log or echo secret values. Runtime passes a bound
// egress.BearerSource to egress.NewClient and never exposes credentials to
// Adapter methods or ModelRequest (runtime-design §4.3, AC-2).
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

// Config configures a Runtime executor.
type Config struct {
	// Registry is the provider Adapter registry. Defaults to provider.DefaultRegistry() when nil.
	Registry *provider.Registry
	// Instances resolves configured provider instances. Optional when callers
	// supply EvalOptions.Instance directly.
	Instances InstanceSource
	// Credentials supplies per-instance bearer API keys at request time.
	Credentials CredentialSource
	// MaxEvalTime is the runtime-wide evaluation ceiling (server.hub.systemone.maxEvalTime;
	// defaults to DefaultMaxEvalTime = 3s when <= 0, clamped to MaxAllowedEvalTime = 30s).
	MaxEvalTime time.Duration
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
	// Consumer, RuleID, and Revision populate Evaluation.Rule.
	Consumer string
	RuleID   string
	Revision int64
	// ProjectID populates Evaluation.ProjectID.
	ProjectID string
	// MaxEvalTime optionally lowers MaxEvalTime for this evaluation (clamped to
	// MaxAllowedEvalTime = 30s; effective ceiling is min(r.MaxEvalTime(),
	// opts.MaxEvalTime), never raising above the Runtime's configured ceiling).
	MaxEvalTime time.Duration
	// Budget optionally caps the evaluation by a remaining aggregate consumer
	// budget when positive (runtime-design §7.1). Budget <= 0 means no budget is
	// set; callers tracking an aggregate budget must enforce exhaustion before
	// calling Evaluate and must never call Evaluate with an exhausted budget.
	Budget time.Duration
}

type transportKey struct {
	id       string
	name     string
	revision int64
}

// Runtime executes compiled System One rules under bounded deadlines and
// content-environment limits, producing typed Decisions and content-free
// Evaluation provenance records (runtime-design §§6, 7.1, 7.7, 8.1).
type Runtime struct {
	registry        *provider.Registry
	instances       InstanceSource
	credentials     CredentialSource
	maxEvalTime     time.Duration
	defaultInstance string
	logger          *slog.Logger
	egressOpts      []egress.Option

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

	r := &Runtime{
		registry:        reg,
		instances:       cfg.Instances,
		credentials:     cfg.Credentials,
		maxEvalTime:     maxEval,
		defaultInstance: cfg.DefaultInstance,
		logger:          logger,
		transports:      make(map[transportKey]provider.Transport),
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

// CloseIdleConnections closes idle HTTP keep-alive connections across cached
// egress transports.
func (r *Runtime) CloseIdleConnections() {
	r.transportMu.Lock()
	defer r.transportMu.Unlock()
	for _, tr := range r.transports {
		tr.CloseIdleConnections()
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

	if compiled == nil {
		eval := &Evaluation{
			EvalID:    evalID,
			Rule:      RuleRef{Consumer: opts.Consumer, RuleID: opts.RuleID, Revision: opts.Revision, Dialect: DialectName},
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
			Consumer:    opts.Consumer,
			RuleID:      opts.RuleID,
			Revision:    opts.Revision,
			ContentHash: compiled.ContentHash,
			Dialect:     DialectName,
			Native:      compiled.Spec.Form == spec.FormNative,
		},
		ProjectID: opts.ProjectID,
		Requested: compiled.Spec.Binding,
	}

	// Compose initial runtime-owned deadline (rule timeout, MaxEvalTime, Budget)
	// before instance resolution; CallTimeout tightens it once inst is resolved.
	baseOwnDur := r.effectiveOwnDuration(compiled.Timeout, opts, 0)
	ownDeadline := start.Add(baseOwnDur)

	initCtx, initCancel := context.WithDeadlineCause(ctx, ownDeadline, errRuntimeDeadline)
	initErr := initCtx.Err()
	initCause := context.Cause(initCtx)
	parentErr := ctx.Err()
	initCancel()
	if initErr != nil || parentErr != nil {
		return r.finishFailed(ctx, compiled.Kind, start, eval, classifyContextCause(parentErr, initCause))
	}

	// Step 1: Select and validate bounded probe environment fields (AC-2, AC-3).
	selectedFields, fp, totalBytes, fieldReason := selectProbeFields(compiled, input)
	eval.Input = fp
	if fieldReason != "" {
		return r.finishFailed(ctx, compiled.Kind, start, eval, fieldReason)
	}

	// Step 2: Resolve eligible instance and adapter (runtime-design §4.5).
	resolveCtx, resolveCancel := context.WithDeadlineCause(ctx, ownDeadline, errRuntimeDeadline)
	inst, adapter, caps, resolveReason := r.resolveBinding(resolveCtx, ctx, compiled, opts)
	resolveCancel()
	if resolveReason != "" {
		return r.finishFailed(ctx, compiled.Kind, start, eval, resolveReason)
	}

	eval.ConfiguredModel = inst.Model

	// Step 3: Conservative bytes-to-tokens pre-check against MaxInputTokens (AC-3).
	if precheckReason := checkMaxInputTokensBytes(totalBytes, caps, inst.Limits); precheckReason != "" {
		return r.finishFailed(ctx, compiled.Kind, start, eval, precheckReason)
	}

	// Step 4: Verify P1 single-request question limit and auth scheme support.
	if caps.MaxQuestionsPerRequest > 0 && len(compiled.Spec.Questions) > caps.MaxQuestionsPerRequest {
		return r.finishFailed(ctx, compiled.Kind, start, eval, provider.FailUnsupported)
	}
	if authReason := checkInstanceAuthSupport(caps.AuthScheme, inst.Auth); authReason != "" {
		return r.finishFailed(ctx, compiled.Kind, start, eval, authReason)
	}

	// Step 5: Tighten runtime-owned deadline with per-instance CallTimeout ceiling (§7.1).
	attemptOwnDur := r.effectiveOwnDuration(compiled.Timeout, opts, inst.Limits.CallTimeout)
	ownDeadline = start.Add(attemptOwnDur)
	checkCtx, checkCancel := context.WithDeadlineCause(ctx, ownDeadline, errRuntimeDeadline)
	checkErr := checkCtx.Err()
	checkCause := context.Cause(checkCtx)
	parentErr = ctx.Err()
	checkCancel()
	if checkErr != nil || parentErr != nil {
		return r.finishFailed(ctx, compiled.Kind, start, eval, classifyContextCause(parentErr, checkCause))
	}

	// Step 6: Build credential-free ModelRequest from compiled spec + selected fields only (AC-2).
	req := buildModelRequest(evalID, compiled, selectedFields)
	instView := inst.View()
	transport := r.transportForInstance(inst)

	attemptStart := time.Now().UTC()
	attemptCtx, attemptCancel := context.WithDeadlineCause(ctx, ownDeadline, errRuntimeDeadline)
	res, invokeErr, panicked := invokeSafely(attemptCtx, adapter, transport, instView, req)
	attemptDuration := time.Since(attemptStart)
	callerErr := ctx.Err()
	attemptErr := attemptCtx.Err()
	attemptCause := context.Cause(attemptCtx)
	attemptCancel()

	attempt := Attempt{
		Instance:          inst.Name,
		InstanceRevision:  inst.Revision,
		Adapter:           adapter.Name(),
		AdapterVersion:    adapter.Version(),
		ConfiguredModel:   inst.Model,
		ReportedModel:     res.ResolvedModel,
		ProviderRequestID: res.ProviderRequestID,
		Location:          inst.Location,
		Started:           attemptStart,
		Duration:          attemptDuration,
		Requests:          1,
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
		attempt.Outcome = AttemptFailed
		attempt.Reason = string(reason)
		eval.Attempts = []Attempt{attempt}
		if res.ResolvedModel != "" {
			eval.ResolvedModel = res.ResolvedModel
		}
		eval.Notes = mergeNotes(eval.Notes, res.Notes)
		return r.finishFailed(ctx, compiled.Kind, start, eval, reason)
	}

	// Even if Adapter.Invoke returned nil error, enforce deadline/cancel if the
	// context expired concurrently before Invoke returned.
	if callerErr != nil || attemptErr != nil {
		reason := classifyContextCause(callerErr, attemptCause)
		attempt.Outcome = AttemptFailed
		attempt.Reason = string(reason)
		eval.Attempts = []Attempt{attempt}
		return r.finishFailed(ctx, compiled.Kind, start, eval, reason)
	}
	eval.ResolvedModel = res.ResolvedModel
	eval.Notes = mergeNotes(eval.Notes, res.Notes)

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
	adapter, ok := r.registry.Get(inst.Adapter)
	if !ok {
		return nil, provider.NewProviderError(provider.FailNoEligibleInstance, false, 0, 0, "adapter is not registered")
	}
	lister, ok := adapter.(provider.ModelLister)
	if !ok {
		return nil, provider.NewProviderError(provider.FailUnsupported, false, 0, 0, "adapter does not implement ModelLister")
	}
	caps, err := adapter.Capabilities(inst.View())
	if err != nil {
		if pe, ok := provider.AsProviderError(err); ok {
			return nil, pe
		}
		return nil, provider.NewProviderError(provider.FailInternal, false, 0, 0, "failed to inspect adapter capabilities")
	}
	if authReason := checkInstanceAuthSupport(caps.AuthScheme, inst.Auth); authReason != "" {
		return nil, provider.NewProviderError(authReason, false, 0, 0, "instance auth scheme is not supported")
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

	models, err := lister.ListModels(callCtx, r.transportForInstance(inst), inst.View())
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
	return models, nil
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

	var inst Instance
	switch {
	case opts.Instance != nil:
		if compiled.Spec.Binding.Instance != "" && opts.Instance.Name != compiled.Spec.Binding.Instance {
			return Instance{}, nil, provider.Capabilities{}, provider.FailNoEligibleInstance
		}
		inst = cloneInstance(*opts.Instance)

	case compiled.Spec.Binding.Instance != "":
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
			if candidate.Enabled {
				enabled = append(enabled, candidate)
			}
		}
		if len(enabled) != 1 {
			return Instance{}, nil, provider.Capabilities{}, provider.FailNoEligibleInstance
		}
		inst = cloneInstance(enabled[0])
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

func (r *Runtime) transportForInstance(inst Instance) provider.Transport {
	key := transportKey{
		id:       inst.ID,
		name:     inst.Name,
		revision: inst.Revision,
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
	creds := r.credentials
	bearer := func(reqCtx context.Context) (string, error) {
		if creds == nil {
			return "", egress.ErrAuth
		}
		return creds(reqCtx, instSnap)
	}
	tr := egress.NewClient(bearer, r.egressOpts...)
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

func selectProbeFields(compiled *spec.Compiled, input Input) ([]provider.InputField, InputFingerprint, int, provider.FailReason) {
	if err := spec.ValidateInputs(compiled.Inputs, compiled.Environment); err != nil {
		return nil, InputFingerprint{}, 0, provider.FailInvalidRequest
	}

	byName := make(map[string]provider.InputField, len(input.Fields))
	for _, f := range input.Fields {
		if _, dup := byName[f.Name]; dup {
			return nil, InputFingerprint{}, 0, provider.FailInvalidRequest
		}
		byName[f.Name] = f
	}

	selected := make([]provider.InputField, 0, len(compiled.Inputs))
	fps := make([]FieldFingerprint, 0, len(compiled.Inputs))
	totalBytes := 0
	oversize := false

	for _, name := range compiled.Inputs {
		decl, ok := compiled.Environment.Field(name)
		if !ok {
			return nil, InputFingerprint{}, 0, provider.FailInvalidRequest
		}
		supplied, present := byName[name]
		if !present {
			return nil, InputFingerprint{}, 0, provider.FailInvalidRequest
		}
		if !utf8.ValidString(supplied.Value) {
			return nil, InputFingerprint{}, 0, provider.FailInvalidRequest
		}

		sz := len(supplied.Value)
		totalBytes += sz
		fps = append(fps, FieldFingerprint{
			Name:      name,
			Bytes:     sz,
			Truncated: false,
		})
		if decl.MaxBytes > 0 && sz > decl.MaxBytes {
			oversize = true
		}
		selected = append(selected, provider.InputField{
			Name:      name,
			Kind:      decl.Kind,
			Value:     supplied.Value,
			Truncated: false,
			Trust:     decl.Trust,
		})
	}

	if compiled.Environment.MaxTotalBytes > 0 && totalBytes > compiled.Environment.MaxTotalBytes {
		oversize = true
	}

	fp := InputFingerprint{
		Fields: fps,
		SHA256: computeSelectedInputSHA256(selected, input.Params, input.ChoiceSets),
	}
	if oversize {
		return nil, fp, totalBytes, provider.FailInputTooLarge
	}
	return selected, fp, totalBytes, ""
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

func checkInstanceAuthSupport(adapterScheme provider.AuthScheme, instAuth InstanceAuth) provider.FailReason {
	switch adapterScheme.Kind {
	case provider.AuthKindBearer:
		switch instAuth.Kind {
		case "", "api_key", string(provider.AuthKindBearer):
			return ""
		default:
			return provider.FailUnsupported
		}
	case provider.AuthKindNone:
		if instAuth.Kind == "" || instAuth.Kind == string(provider.AuthKindNone) {
			return ""
		}
		return provider.FailUnsupported
	default:
		return provider.FailUnsupported
	}
}

func buildModelRequest(evalID string, compiled *spec.Compiled, selectedFields []provider.InputField) provider.ModelRequest {
	spCopy := cloneSpec(compiled.Spec)
	qIDs := make([]string, len(spCopy.Questions))
	for i, q := range spCopy.Questions {
		qIDs[i] = q.ID
	}
	inCopy := make([]provider.InputField, len(selectedFields))
	copy(inCopy, selectedFields)
	return provider.ModelRequest{
		Rule:      spCopy,
		Questions: qIDs,
		Input:     inCopy,
		RequestID: evalID,
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
	// (FailMalformedResponse); credential errors map to FailAuth.
	switch {
	case egress.IsResponseTooLarge(err):
		return provider.FailMalformedResponse, statusClass
	case egress.IsAuthError(err):
		return provider.FailAuth, statusClass
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

func mergeNotes(existing []string, added []string) []string {
	out := existing
	for _, n := range added {
		if n == "" || slices.Contains(out, n) {
			continue
		}
		out = append(out, n)
	}
	return out
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
