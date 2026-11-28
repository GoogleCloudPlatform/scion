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

package ruleengine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
)

// Engine memory-safety ceilings for chain execution (v1 §3.1, §3.6).
const (
	MaxChainElementsCeiling   = 64
	MaxChainTraceBytesCeiling = 64 * 1024
)

// Domain binds a captured context and a Budget for one execution domain
// (Inline vs IO, Amendment A1 §3).
type Domain struct {
	Ctx    context.Context // captured at construction; immutable for the run
	Budget Budget          // Tracker == nil means untracked
}

// ChainDomains specifies the Inline and IO execution domains for a ChainRun
// (Amendment A1 §3). A consumer wanting one regime passes the same Domain for
// both fields.
type ChainDomains struct {
	Inline Domain // elements whose dialect Declares().PerformsIO == false
	IO     Domain // elements whose dialect Declares().PerformsIO == true
}

// ChainElementSource allows an Inline (PerformsIO == false) chain element to be
// supplied in source form and compiled on reach via CachedCompile (Amendment A1 §4).
// Supplying Source for an IO element is a construction error (P18).
type ChainElementSource struct {
	Expr       string
	Scope      celengine.CacheScope
	Kind       Kind
	EnvRef     EnvironmentRef
	EnvVersion int // optional environment version included in stable program identity

	// Optional consumer-supplied separate compile budget and miss observer.
	// When CompileBudget is nil (the default, A1 §4 / P12), compilation runs
	// inside the element's dispatch-time bound and is charged to the element's
	// domain tracker. When CompileBudget is non-nil (e.g. triggers SEC-3b2-01),
	// cache-miss compilations are gated and charged to CompileBudget instead.
	CompileBudget *BudgetTracker
	OnCompileMiss func(dialect string)
}

// ComputeSourceProgramID returns the stable content hash of
// {dialect, expr, env.Name, envVersion, scope, kind} before compilation (A1 §4).
func ComputeSourceProgramID(dialect string, src ChainElementSource) string {
	h := sha256.New()
	writePart := func(label, val string) {
		_, _ = fmt.Fprintf(h, "%s=%d:%s\n", label, len(val), val)
	}
	writePart("dialect", normalizeDialect(dialect))
	writePart("expr", src.Expr)
	writePart("consumer", strings.TrimSpace(src.EnvRef.Consumer))
	writePart("env", src.EnvRef.Name)
	writePart("env_ver", fmt.Sprintf("%d", src.EnvVersion))
	writePart("kind", string(src.Kind))
	writePart("scope_consumer", strings.TrimSpace(src.Scope.Consumer))
	writePart("scope_project", src.Scope.ProjectID)
	writePart("scope_id", src.Scope.ID)
	writePart("scope_rev", fmt.Sprintf("%d", src.Scope.Revision))
	return hex.EncodeToString(h.Sum(nil))
}

// ChainElement is a single ordered step in a Chain (v1 §3.1, A1 §4).
type ChainElement struct {
	Index    int
	Dialect  string
	Kind     Kind
	Compiled Compiled
	Source   *ChainElementSource // Inline-only; nil when Compiled is provided
	Role     string              // consumer token, opaque to the engine
	GroupKey string              // consumer-computed, opaque; "" = never grouped
}

// ProgramID returns the element's stable semantic program identity before or
// after compilation, or "" when a Compiled handle declares no explicit ProgramID.
func (el ChainElement) ProgramID() string {
	if !el.Compiled.IsZero() {
		return el.Compiled.ProgramID()
	}
	if el.Source != nil {
		return ComputeSourceProgramID(el.Dialect, *el.Source)
	}
	return ""
}

// Chain is an ordered list of ChainElements evaluated strictly in author order.
type Chain struct {
	Elements []ChainElement
}

// ChainLimits specifies consumer-supplied caps for a ChainRun. The engine
// additionally enforces MaxChainElementsCeiling and MaxChainTraceBytesCeiling.
type ChainLimits struct {
	MaxElements   int
	MaxTraceBytes int
}

// StepClass is the closed engine-owned outcome classification for a chain step.
// Consumer decisions/verdicts are always kept separate in Control.Verdict.
type StepClass string

const (
	StepClassPass      StepClass = "pass"
	StepClassFail      StepClass = "fail"
	StepClassUncertain StepClass = "uncertain"
	StepClassFailed    StepClass = "failed"
	StepClassBudget    StepClass = "budget"
	StepClassCanceled  StepClass = "canceled"
	StepClassSkipped   StepClass = "skipped"
)

// StepOutcome records the engine-owned result of evaluating one chain element.
type StepOutcome struct {
	Index        int
	Dialect      string
	Kind         Kind
	Class        StepClass
	Reason       string          // closed token (engine set ∪ dialect TraceReasons)
	Elapsed      time.Duration   // charged evaluation elapsed (0 for non-leader group members)
	GroupElapsed time.Duration   // group execution elapsed visible to every member of a group execution (0 when evaluated alone)
	GroupID      string          // "" when evaluated alone; identical for all members of one group execution
	Domain       ChainDomainName // "inline" | "io"
	Provenance   map[string]any  // validated typed operational metadata
	RawTrace     string          // optional legacy Decision.Trace (not serialized by ChainTrace)
	Err          error           // underlying error for non-pass/fail steps (not serialized by ChainTrace)
}

// Control is the consumer's routing decision returned by StepPolicy.
// Verdict is opaque to the engine and kept strictly separate from StepClass.
type Control struct {
	Stop    bool
	Verdict string
}

// StepPolicy decides whether a ChainRun stops or continues after a StepOutcome.
// A nil StepPolicy defaults to DefaultANDPolicy.
type StepPolicy func(out StepOutcome) Control

// DefaultANDPolicy continues on StepClassPass and stops on any other class.
func DefaultANDPolicy(out StepOutcome) Control {
	if out.Class == StepClassPass {
		return Control{Stop: false}
	}
	return Control{Stop: true}
}

// Judgment is an immutable, deep-copied projection of a KindJudgment step's
// Decision.Detail.
type Judgment struct {
	Step    int
	Dialect string
	Detail  any
}

// JudgmentView provides read-only, defensive-copy access to judgments produced
// by earlier steps (strictly < current step index).
type JudgmentView interface {
	Get(step int) (Judgment, bool)
	Steps() []int
}

// Enricher supplies per-step additions under the "chain" namespace (and any
// consumer-declared reserved judgment-carry names, A1 §5). It cannot touch,
// shadow, or replace base input bindings.
type Enricher interface {
	Additions(step int, j JudgmentView) (map[string]any, error)
}

// NoopEnricher is an Enricher that supplies no additions.
type NoopEnricher struct{}

// Additions implements Enricher with no additions.
func (NoopEnricher) Additions(int, JudgmentView) (map[string]any, error) {
	return nil, nil
}

// ChainResult summarizes the outcome of a ChainRun.
type ChainResult struct {
	Through    int
	Stopped    bool
	Verdict    string
	Deciding   StepOutcome
	Steps      []StepOutcome
	Unexecuted []int
}

// PendingStep represents a reached I/O chain element awaiting engine evaluation
// via EvaluatePending or EvaluateGroup. It carries no deadline (bound is
// computed at dispatch) and is guarded by a single-use, run-and-step-bound token.
//
// Lock discipline (RV2-1): PendingStep has no mutex of its own. The mutable
// field consumed is guarded exclusively by p.run.mu, eliminating any possible
// lock-order inversion between ChainRun and PendingStep.
type PendingStep struct {
	run                *ChainRun
	elemPos            int
	index              int
	dialect            string
	kind               Kind
	compiled           Compiled
	input              Input
	admittedInputHash  string
	admittedInputValid bool
	groupKey           string
	tokenID            uint64

	// consumed is guarded by run.mu (see ChainRun.mu lock discipline).
	consumed bool
}

// Index returns the step index of the pending element.
func (p *PendingStep) Index() int {
	if p == nil {
		return 0
	}
	return p.index
}

// GroupKey returns the consumer-computed grouping key ("" means never grouped).
func (p *PendingStep) GroupKey() string {
	if p == nil {
		return ""
	}
	return p.groupKey
}

// Dialect returns the normalized dialect name of the pending element.
func (p *PendingStep) Dialect() string {
	if p == nil {
		return ""
	}
	return p.dialect
}

type chainRunState int

const (
	chainStateReady chainRunState = iota
	chainStateEvaluating
	chainStatePending
	chainStatePendingCompleted
	chainStateDone
)

// ChainRun is a resumable, single-run ordered rule-chain execution owned by Engine.
//
// A ChainRun is single-consumer: callers drive one step at a time (Next ->
// optional EvaluatePending/EvaluateGroup -> Resume). Calling Next or Resume
// concurrently while an evaluation or pending step is in progress returns an
// error without modifying state or charging any budget (RV2-2).
//
// Lock discipline (RV2-1): mu is the sole mutex protecting both ChainRun state
// and its active PendingStep.consumed flag. No code path ever acquires two
// ChainRun.mu locks simultaneously.
type ChainRun struct {
	mu       sync.Mutex
	engine   *Engine
	domains  ChainDomains
	elements []chainElemSpec
	base     Input
	limits   ChainLimits
	policy   StepPolicy
	enricher Enricher
	reserved map[string]struct{}

	state          chainRunState
	nextPos        int
	tokenSeq       uint64
	pending        *PendingStep
	pendingOutcome StepOutcome
	pendingDetail  any
	pendingCopier  func(any) any

	judgments map[int]storedJudgment
	steps     []StepOutcome
	trace     []ChainTraceElement
	stopped   bool
	verdict   string
	deciding  StepOutcome
}

type storedJudgment struct {
	judgment Judgment
	copier   func(any) any
}

type chainElemSpec struct {
	elem       ChainElement
	dialect    string
	consumer   string
	performsIO bool
	programID  string
}

var reservedEngineRoots = map[string]bool{
	"event":   true,
	"trigger": true,
	"chain":   true,
}

func isValidIdentifier(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i := 0; i < len(s); i++ {
		b := s[i]
		if i == 0 {
			if (b < 'a' || b > 'z') && (b < 'A' || b > 'Z') && b != '_' {
				return false
			}
			continue
		}
		if (b < 'a' || b > 'z') && (b < 'A' || b > 'Z') && (b < '0' || b > '9') && b != '_' {
			return false
		}
	}
	return true
}

// NewChainRun constructs an immutable, resumable ChainRun over c using domains d
// (Amendment A1 §3–§5, v1 §3.1–§3.2). Construction fails closed on nil domain
// contexts, element cap violations, invalid/colliding reserved names, un-admitted
// dialects, IO elements supplied in Source form (P14, P15, P18), KindJudgment
// elements whose dialect declares no CopyDetail (R-3), non-increasing element
// indexes (N-2), or base.Activation containing non-JSON-shaped / non-copyable
// container types (O-b).
//
// base.Activation values must be JSON-shaped primitives, time.Time/time.Duration,
// or standard map/slice container types (map[string]any, map[string]string,
// map[string]bool, map[string]float64, map[string][]string, []any, []string,
// []int, []bool, []float64, []byte) so the engine can capture an alias-free
// immutable copy once at construction.
func (e *Engine) NewChainRun(
	d ChainDomains,
	c Chain,
	base Input,
	lim ChainLimits,
	policy StepPolicy,
	enr Enricher,
	reserved []string,
) (*ChainRun, error) {
	if e == nil {
		return nil, errors.New("ruleengine: nil Engine")
	}
	if d.Inline.Ctx == nil {
		return nil, errors.New("ruleengine: Inline domain Ctx must not be nil")
	}
	if d.IO.Ctx == nil {
		return nil, errors.New("ruleengine: IO domain Ctx must not be nil")
	}

	maxElems := MaxChainElementsCeiling
	if lim.MaxElements > 0 && lim.MaxElements < maxElems {
		maxElems = lim.MaxElements
	}
	if len(c.Elements) > maxElems {
		return nil, fmt.Errorf("ruleengine: chain element count %d exceeds maximum %d", len(c.Elements), maxElems)
	}
	maxTraceBytes := MaxChainTraceBytesCeiling
	if lim.MaxTraceBytes > 0 && lim.MaxTraceBytes < maxTraceBytes {
		maxTraceBytes = lim.MaxTraceBytes
	}

	// Validate reserved judgment-carry names (Amendment A1 §5, D4, P10, P15).
	reservedSet := make(map[string]struct{}, len(reserved))
	for _, rawName := range reserved {
		if !isValidIdentifier(rawName) {
			return nil, fmt.Errorf("ruleengine: invalid reserved name %q: must be an identifier", rawName)
		}
		if reservedEngineRoots[rawName] {
			return nil, fmt.Errorf("ruleengine: reserved name %q collides with engine root", rawName)
		}
		if _, dup := reservedSet[rawName]; dup {
			return nil, fmt.Errorf("ruleengine: duplicate reserved name %q", rawName)
		}
		if base.Activation != nil {
			if _, exists := base.Activation[rawName]; exists {
				return nil, fmt.Errorf("ruleengine: base input must not contain reserved name %q", rawName)
			}
		}
		reservedSet[rawName] = struct{}{}
	}

	specs := make([]chainElemSpec, len(c.Elements))
	for i, el := range c.Elements {
		hasCompiled := !el.Compiled.IsZero()
		hasSource := el.Source != nil
		if hasCompiled == hasSource {
			return nil, fmt.Errorf("ruleengine: chain element %d must specify exactly one of Compiled or Source", i)
		}

		var dialect, consumer, progID string
		var kind Kind
		if hasSource {
			src := *el.Source
			dialect = normalizeDialect(el.Dialect)
			consumer = strings.TrimSpace(src.EnvRef.Consumer)
			kind = el.Kind
			if kind == "" {
				kind = src.Kind
			}
			if src.Kind != "" && src.Kind != kind {
				return nil, fmt.Errorf("ruleengine: chain element %d Kind %q disagrees with Source.Kind %q", i, kind, src.Kind)
			}
			src.Kind = kind
			if src.Scope.Consumer == "" {
				src.Scope.Consumer = consumer
			}
			el.Source = &src
		} else {
			dialect = normalizeDialect(el.Compiled.Dialect())
			if el.Dialect != "" && normalizeDialect(el.Dialect) != dialect {
				return nil, fmt.Errorf("ruleengine: chain element %d Dialect %q disagrees with Compiled.Dialect %q", i, el.Dialect, dialect)
			}
			consumer = el.Compiled.Consumer()
			kind = el.Kind
			if kind == "" {
				kind = el.Compiled.Kind()
			}
			if el.Compiled.Kind() != "" && el.Compiled.Kind() != kind {
				return nil, fmt.Errorf("ruleengine: chain element %d Kind %q disagrees with Compiled.Kind %q", i, kind, el.Compiled.Kind())
			}
			progID = el.Compiled.ProgramID()
		}

		if kind != KindFilter && kind != KindSubject && kind != KindJudgment {
			return nil, fmt.Errorf("ruleengine: chain element %d has invalid kind %q", i, kind)
		}

		dial, err := e.resolveDialect(consumer, dialect)
		if err != nil {
			return nil, fmt.Errorf("ruleengine: chain element %d admission failed: %w", i, err)
		}
		caps := dial.Declares()
		performsIO := caps.PerformsIO
		if hasSource && performsIO {
			return nil, fmt.Errorf("ruleengine: chain element %d dialect %q performs IO and cannot use Source form", i, dialect)
		}
		// R-3 option (b): KindJudgment elements require a dialect-declared
		// concrete-type-preserving CopyDetail copier.
		if kind == KindJudgment && caps.CopyDetail == nil {
			return nil, fmt.Errorf("ruleengine: chain element %d dialect %q performs KindJudgment but declares no Capabilities.CopyDetail", i, dialect)
		}
		// N-1: populate EnvVersion from registered CELDialect when unset so
		// pre-compile Source ProgramID matches post-compile CELDialect ProgramID.
		if hasSource {
			if el.Source.EnvVersion == 0 {
				if cd, ok := dial.(*CELDialect); ok {
					envName := el.Source.EnvRef.Name
					if envName == "" {
						envName = el.Source.EnvRef.Consumer
					}
					if celEng, found := cd.CELEngine(envName); found {
						el.Source.EnvVersion = celEng.Environment().Version
					}
				}
			}
			progID = ComputeSourceProgramID(dialect, *el.Source)
		}

		// N-2: element indexes must be non-negative, unique, and strictly increasing.
		if el.Index < 0 {
			return nil, fmt.Errorf("ruleengine: chain element %d has negative index %d", i, el.Index)
		}
		if i > 0 && el.Index <= specs[i-1].elem.Index {
			return nil, fmt.Errorf("ruleengine: chain element indexes must be strictly increasing (pos %d has index %d <= %d)", i, el.Index, specs[i-1].elem.Index)
		}

		el.Dialect = dialect
		el.Kind = kind
		specs[i] = chainElemSpec{
			elem:       el,
			dialect:    dialect,
			consumer:   consumer,
			performsIO: performsIO,
			programID:  progID,
		}
	}

	if policy == nil {
		policy = DefaultANDPolicy
	}

	// O-5 & O-b: defensive copy of base Activation once at construction, shared
	// read-only across steps. Rejects non-copyable container/pointer/struct types
	// so caller mutations can never alias into the run.
	actCopy, err := deepCopyActivationMap(base.Activation)
	if err != nil {
		return nil, fmt.Errorf("ruleengine: base activation must be JSON-shaped: %w", err)
	}
	baseCopy := Input{
		Activation:       actCopy,
		Payload:          base.Payload,
		AdmittedInputKey: base.AdmittedInputKey,
	}
	domainsCopy := ChainDomains{
		Inline: Domain{Ctx: d.Inline.Ctx, Budget: d.Inline.Budget},
		IO:     Domain{Ctx: d.IO.Ctx, Budget: d.IO.Budget},
	}

	run := &ChainRun{
		engine:    e,
		domains:   domainsCopy,
		elements:  specs,
		base:      baseCopy,
		limits:    ChainLimits{MaxElements: maxElems, MaxTraceBytes: maxTraceBytes},
		policy:    policy,
		enricher:  enr,
		reserved:  reservedSet,
		state:     chainStateReady,
		judgments: make(map[int]storedJudgment),
		steps:     make([]StepOutcome, 0, len(specs)),
		trace:     make([]ChainTraceElement, 0, len(specs)),
	}
	if len(specs) == 0 {
		run.state = chainStateDone
	}
	return run, nil
}

// Next advances the run to the next reached element. Non-I/O elements are
// evaluated inline through the engine's ordinary evaluation path; I/O elements
// are not evaluated and instead park the run and return a PendingStep.
func (r *ChainRun) Next() (*StepOutcome, *PendingStep, bool, error) {
	r.mu.Lock()
	if r.state == chainStateDone {
		r.mu.Unlock()
		return nil, nil, true, nil
	}
	if r.state == chainStateEvaluating {
		r.mu.Unlock()
		return nil, nil, false, errors.New("ruleengine: cannot call Next while an evaluation is already in progress")
	}
	if r.state == chainStatePending || r.state == chainStatePendingCompleted {
		r.mu.Unlock()
		return nil, nil, false, errors.New("ruleengine: cannot call Next while a pending step is unresolved")
	}
	if r.nextPos >= len(r.elements) {
		r.state = chainStateDone
		r.mu.Unlock()
		return nil, nil, true, nil
	}

	spec := r.elements[r.nextPos]
	domName := DomainInline
	if spec.performsIO {
		domName = DomainIO
	}

	// D1 / D2: Inline.Ctx is the run context; if canceled, terminate immediately
	// with zero further evaluation or pending steps.
	if ctxErr := r.domains.Inline.Ctx.Err(); ctxErr != nil {
		out := StepOutcome{
			Index:   spec.elem.Index,
			Dialect: spec.dialect,
			Kind:    spec.elem.Kind,
			Class:   StepClassCanceled,
			Reason:  celengine.ReasonCanceled,
			Domain:  domName,
			Err:     &FailedError{Reason: celengine.ReasonCanceled, Cause: ctxErr},
		}
		r.recordStepLocked(out, Control{Stop: true}, nil, nil)
		r.state = chainStateDone
		r.mu.Unlock()
		cp := cloneStepOutcome(out)
		return &cp, nil, true, nil
	}

	// Per-element dialect re-admission at evaluation time (§3.7 item 5).
	dial, err := r.engine.resolveDialect(spec.consumer, spec.dialect)
	if err != nil {
		var caps Capabilities
		reason := classifyErrorReason(err, StepClassFailed, caps)
		out := StepOutcome{
			Index:   spec.elem.Index,
			Dialect: spec.dialect,
			Kind:    spec.elem.Kind,
			Class:   StepClassFailed,
			Reason:  reason,
			Domain:  domName,
			Err:     err,
		}
		ctrl := r.policy(cloneStepOutcome(out))
		r.recordStepLocked(out, ctrl, nil, nil)
		r.nextPos++
		done := ctrl.Stop || r.nextPos >= len(r.elements)
		if done {
			r.state = chainStateDone
		}
		r.mu.Unlock()
		cp := cloneStepOutcome(out)
		return &cp, nil, done, nil
	}
	caps := dial.Declares()

	// Build step input = immutable base ⊎ validated Enricher.Additions(step, view).
	stepInput, enrichErr := r.resolveStepInputLocked(spec.elem.Index)
	if enrichErr != nil {
		out := StepOutcome{
			Index:   spec.elem.Index,
			Dialect: spec.dialect,
			Kind:    spec.elem.Kind,
			Class:   StepClassFailed,
			Reason:  ReasonInvalidEnrichment,
			Domain:  domName,
			Err:     enrichErr,
		}
		ctrl := r.policy(cloneStepOutcome(out))
		r.recordStepLocked(out, ctrl, nil, nil)
		r.nextPos++
		done := ctrl.Stop || r.nextPos >= len(r.elements)
		if done {
			r.state = chainStateDone
		}
		r.mu.Unlock()
		cp := cloneStepOutcome(out)
		return &cp, nil, done, nil
	}

	// I/O element: park and yield PendingStep with single-use token and NO deadline.
	if caps.PerformsIO {
		r.tokenSeq++
		inHash, hashErr := computeAdmittedInputHash(stepInput)
		p := &PendingStep{
			run:                r,
			elemPos:            r.nextPos,
			index:              spec.elem.Index,
			dialect:            spec.dialect,
			kind:               spec.elem.Kind,
			compiled:           spec.elem.Compiled,
			input:              stepInput,
			admittedInputHash:  inHash,
			admittedInputValid: hashErr == nil,
			groupKey:           spec.elem.GroupKey,
			tokenID:            r.tokenSeq,
		}
		r.state = chainStatePending
		r.pending = p
		r.mu.Unlock()
		return nil, p, false, nil
	}

	// Inline element: mark evaluation in progress (RV2-2) before releasing
	// r.mu so any concurrent Next/Resume returns an error without mutating state.
	inlineDomain := r.domains.Inline
	r.state = chainStateEvaluating
	r.mu.Unlock()

	out, detail := r.engine.evaluateInlineElement(inlineDomain, spec, caps, stepInput)

	r.mu.Lock()
	ctrl := r.policy(cloneStepOutcome(out))
	// Engine-owned terminality (§3.2): budget and canceled always end the run.
	if out.Class == StepClassBudget || out.Class == StepClassCanceled {
		ctrl.Stop = true
	}
	r.recordStepLocked(out, ctrl, detail, caps.CopyDetail)
	r.nextPos++
	done := ctrl.Stop || r.nextPos >= len(r.elements)
	if done {
		r.state = chainStateDone
	} else {
		r.state = chainStateReady
	}
	r.mu.Unlock()

	cp := cloneStepOutcome(out)
	return &cp, nil, done, nil
}

// evaluateInlineElement evaluates a single Inline (PerformsIO == false) element
// in either Compiled or Source form under inlineDomain.
func (e *Engine) evaluateInlineElement(
	dom Domain,
	spec chainElemSpec,
	caps Capabilities,
	input Input,
) (StepOutcome, any) {
	if ctxErr := dom.Ctx.Err(); ctxErr != nil {
		return StepOutcome{
			Index:   spec.elem.Index,
			Dialect: spec.dialect,
			Kind:    spec.elem.Kind,
			Class:   StepClassCanceled,
			Reason:  celengine.ReasonCanceled,
			Domain:  DomainInline,
			Err:     &FailedError{Reason: celengine.ReasonCanceled, Cause: ctxErr},
		}, nil
	}
	if dom.Budget.Tracker != nil && dom.Budget.Tracker.Exhausted() {
		return StepOutcome{
			Index:   spec.elem.Index,
			Dialect: spec.dialect,
			Kind:    spec.elem.Kind,
			Class:   StepClassBudget,
			Reason:  ReasonBudgetExhausted,
			Domain:  DomainInline,
			Err:     ErrBudgetExhausted,
		}, nil
	}

	var (
		dec     Decision
		elapsed time.Duration
		evalErr error
	)
	if spec.elem.Source != nil {
		dec, elapsed, evalErr = e.compileAndEvaluateSource(dom, spec, caps, input)
	} else {
		dec, elapsed, evalErr = e.evaluateCore(dom.Ctx, spec.elem.Compiled, input, dom.Budget)
	}

	out := classifyDecisionToOutcome(spec.elem.Index, spec.dialect, spec.elem.Kind, DomainInline, "", caps, dec, elapsed, evalErr)
	var detail any
	if spec.elem.Kind == KindJudgment && out.Class == StepClassPass {
		detail = copyJudgmentDetail(dec.Detail, caps.CopyDetail)
	}
	return out, detail
}

// compileAndEvaluateSource compiles a Source-form Inline element on reach via
// CachedCompile inside the per-element bound, charging the domain tracker
// (Amendment A1 §4, P12, P17), or gating/charging a consumer-supplied
// CompileBudget when present.
func (e *Engine) compileAndEvaluateSource(
	dom Domain,
	spec chainElemSpec,
	caps Capabilities,
	input Input,
) (Decision, time.Duration, error) {
	src := spec.elem.Source

	// Separate compile-budget path (used by trigger adapter SEC-3b2-01 / O2).
	if src.CompileBudget != nil || src.OnCompileMiss != nil {
		var beforeCompile func() error
		if src.CompileBudget != nil {
			beforeCompile = func() error {
				if src.CompileBudget.Exhausted() {
					return errSourceCompileBudgetExhausted
				}
				return nil
			}
		}
		compileStart := time.Now()
		compiled, hit, err := e.CachedCompileWithGate(dom.Ctx, spec.dialect, src.Scope, src.Expr, src.Kind, src.EnvRef, beforeCompile)
		if !hit && !errors.Is(err, errSourceCompileBudgetExhausted) {
			if src.CompileBudget != nil {
				src.CompileBudget.Charge(max(time.Since(compileStart), time.Duration(0)))
			}
			if src.OnCompileMiss != nil {
				src.OnCompileMiss(spec.dialect)
			}
		}
		if err != nil {
			return Decision{}, 0, &sourceCompileError{cause: err}
		}
		return e.evaluateCore(dom.Ctx, compiled, input, dom.Budget)
	}

	// Default A1 §4 path: compile + evaluate under ONE dispatch-time bound,
	// charged to dom.Budget.Tracker.
	evalTimeout := caps.MaxEvalTime
	boundFromTracker := false
	if dom.Budget.MaxEvalTime > 0 && (evalTimeout == 0 || dom.Budget.MaxEvalTime < evalTimeout) {
		evalTimeout = dom.Budget.MaxEvalTime
	}
	if dom.Budget.Tracker != nil {
		rem := dom.Budget.Tracker.Remaining()
		if rem <= 0 {
			return Decision{}, 0, ErrBudgetExhausted
		}
		if evalTimeout == 0 || rem < evalTimeout {
			evalTimeout = rem
			boundFromTracker = true
		}
	}

	start := time.Now()
	evalCtx := dom.Ctx
	var cancel context.CancelFunc
	if evalTimeout > 0 {
		cause := errDialectEvalTimeout
		if boundFromTracker {
			cause = errTrackerTimeout
		}
		evalCtx, cancel = context.WithTimeoutCause(dom.Ctx, evalTimeout, cause)
		defer cancel()
	}

	compiled, err := e.CachedCompile(evalCtx, spec.dialect, src.Scope, src.Expr, src.Kind, src.EnvRef)
	compileElapsed := max(time.Since(start), time.Duration(0))
	if err != nil {
		if dom.Budget.Tracker != nil {
			dom.Budget.Tracker.Charge(compileElapsed)
		}
		if callerErr := dom.Ctx.Err(); callerErr != nil {
			return Decision{}, compileElapsed, &FailedError{Reason: celengine.ReasonCanceled, Cause: errors.Join(callerErr, err)}
		}
		if evalTimeout > 0 {
			if boundFromTracker && (errors.Is(context.Cause(evalCtx), errTrackerTimeout) || compileElapsed >= evalTimeout-EngineDeadlineMargin(evalTimeout)) {
				dom.Budget.Tracker.Exhaust()
				return Decision{}, compileElapsed, fmt.Errorf("%w: %w", ErrBudgetExhausted, err)
			}
			if errors.Is(context.Cause(evalCtx), errDialectEvalTimeout) {
				return Decision{}, compileElapsed, &FailedError{Reason: celengine.ReasonTimeout, Cause: err}
			}
		}
		return Decision{}, compileElapsed, &sourceCompileError{cause: err}
	}

	// Check if compile consumed the entire dispatch-time bound before evaluating.
	if callerErr := dom.Ctx.Err(); callerErr != nil {
		if dom.Budget.Tracker != nil {
			dom.Budget.Tracker.Charge(compileElapsed)
		}
		return Decision{}, compileElapsed, &FailedError{Reason: celengine.ReasonCanceled, Cause: callerErr}
	}
	if evalTimeout > 0 && evalCtx.Err() != nil {
		if dom.Budget.Tracker != nil {
			dom.Budget.Tracker.Charge(compileElapsed)
		}
		if boundFromTracker {
			dom.Budget.Tracker.Exhaust()
			return Decision{}, compileElapsed, fmt.Errorf("%w: %w", ErrBudgetExhausted, context.Cause(evalCtx))
		}
		return Decision{}, compileElapsed, &FailedError{Reason: celengine.ReasonTimeout, Cause: context.Cause(evalCtx)}
	}

	// Evaluate under the remaining portion of the SAME dispatch-time bound,
	// charging compile + evaluate together to dom.Budget.Tracker.
	effBudget := dom.Budget
	effBudget.Tracker = nil // we charge total elapsed once below
	if evalTimeout > 0 {
		rem := evalTimeout - compileElapsed
		if rem <= 0 {
			if dom.Budget.Tracker != nil {
				dom.Budget.Tracker.Charge(compileElapsed)
			}
			if boundFromTracker {
				dom.Budget.Tracker.Exhaust()
				return Decision{}, compileElapsed, ErrBudgetExhausted
			}
			return Decision{}, compileElapsed, &FailedError{Reason: celengine.ReasonTimeout, Cause: context.DeadlineExceeded}
		}
		effBudget.MaxEvalTime = rem
	}

	dec, _, evalErr := e.evaluateCore(evalCtx, compiled, input, effBudget)
	totalElapsed := max(time.Since(start), time.Duration(0))
	if dom.Budget.Tracker != nil {
		dom.Budget.Tracker.Charge(totalElapsed)
	}
	if evalErr != nil {
		if callerErr := dom.Ctx.Err(); callerErr != nil {
			return Decision{}, totalElapsed, &FailedError{Reason: celengine.ReasonCanceled, Cause: errors.Join(callerErr, evalErr)}
		}
		if evalTimeout > 0 {
			trackerDeadline := errors.Is(context.Cause(evalCtx), errTrackerTimeout) ||
				totalElapsed >= evalTimeout-EngineDeadlineMargin(evalTimeout)
			if boundFromTracker && trackerDeadline && isTimeoutOrCancelFailure(evalErr) {
				dom.Budget.Tracker.Exhaust()
				return Decision{}, totalElapsed, fmt.Errorf("%w: %w", ErrBudgetExhausted, evalErr)
			}
			if errors.Is(context.Cause(evalCtx), errDialectEvalTimeout) && isTimeoutOrCancelFailure(evalErr) {
				return Decision{}, totalElapsed, &FailedError{Reason: celengine.ReasonTimeout, Cause: evalErr}
			}
		}
		return Decision{}, totalElapsed, evalErr
	}
	return dec, totalElapsed, nil
}

var errSourceCompileBudgetExhausted = errors.New("ruleengine: compile budget exhausted")

type sourceCompileError struct {
	cause error
}

func (e *sourceCompileError) Error() string { return "rule compile error: " + e.cause.Error() }
func (e *sourceCompileError) Unwrap() error { return e.cause }

// EvaluatePending evaluates a single PendingStep alone through the engine's
// ordinary evaluation path and completes its single-use token (v1 §3.1, §3.2).
func (e *Engine) EvaluatePending(p *PendingStep) error {
	if e == nil {
		return errors.New("ruleengine: nil Engine")
	}
	if p == nil || p.run == nil {
		return &FailedError{Reason: ReasonInvalidCompletion, Cause: errors.New("nil pending step")}
	}
	if p.run.engine != e {
		return &FailedError{Reason: ReasonInvalidCompletion, Cause: errors.New("pending step belongs to another engine")}
	}

	if err := p.claimForCompletion(); err != nil {
		p.run.failInvalidCompletion(p, err)
		return err
	}

	run := p.run
	run.mu.Lock()
	ioDomain := run.domains.IO
	inlineCtx := run.domains.Inline.Ctx
	run.mu.Unlock()

	// Re-check per-consumer dialect admission at evaluation (§3.7 item 5).
	dial, admErr := e.resolveDialect(p.compiled.Consumer(), p.compiled.Dialect())
	if admErr != nil {
		out := StepOutcome{
			Index:   p.index,
			Dialect: p.dialect,
			Kind:    p.kind,
			Class:   StepClassFailed,
			Reason:  classifyErrorReason(admErr, StepClassFailed, Capabilities{}),
			Domain:  DomainIO,
			Err:     admErr,
		}
		run.completePending(p, out, nil, nil)
		return nil
	}
	caps := dial.Declares()

	// Cancel-at-barrier / D2 cascade check before any I/O.
	if ctxErr := firstCtxErr(inlineCtx, ioDomain.Ctx); ctxErr != nil {
		out := StepOutcome{
			Index:   p.index,
			Dialect: p.dialect,
			Kind:    p.kind,
			Class:   StepClassCanceled,
			Reason:  celengine.ReasonCanceled,
			Domain:  DomainIO,
			Err:     &FailedError{Reason: celengine.ReasonCanceled, Cause: ctxErr},
		}
		run.completePending(p, out, nil, nil)
		return nil
	}

	// Exhausted tracker at dispatch → budget, 0 calls.
	if ioDomain.Budget.Tracker != nil && ioDomain.Budget.Tracker.Exhausted() {
		out := StepOutcome{
			Index:   p.index,
			Dialect: p.dialect,
			Kind:    p.kind,
			Class:   StepClassBudget,
			Reason:  ReasonBudgetExhausted,
			Domain:  DomainIO,
			Err:     ErrBudgetExhausted,
		}
		run.completePending(p, out, nil, nil)
		return nil
	}

	// Wire D2 cascade from Inline.Ctx into IO.Ctx without leaking goroutines.
	evalCtx, stopCascade := cascadeCancelContext(ioDomain.Ctx, inlineCtx)
	defer stopCascade()

	dec, elapsed, evalErr := e.evaluateCore(evalCtx, p.compiled, p.input, ioDomain.Budget)
	out := classifyDecisionToOutcome(p.index, p.dialect, p.kind, DomainIO, "", caps, dec, elapsed, evalErr)
	var detail any
	if p.kind == KindJudgment && out.Class == StepClassPass {
		detail = copyJudgmentDetail(dec.Detail, caps.CopyDetail)
	}
	run.completePending(p, out, detail, caps.CopyDetail)
	return nil
}

// EvaluateGroup validates leader and members as a whole before any I/O, executes
// ONE ordinary evaluation on leader's compiled rule and request, charges the
// shared IO tracker once, and distributes the identical StepOutcome (with
// independent deep-copied judgment Detail) to leader and every member
// (v1 §3.3, §3.10 Addendum A, A1 §3 rule 8).
func (e *Engine) EvaluateGroup(leader *PendingStep, members []*PendingStep) (string, error) {
	if e == nil {
		return "", errors.New("ruleengine: nil Engine")
	}
	if leader == nil || leader.run == nil {
		return "", &FailedError{Reason: ReasonIncompatibleGroup, Cause: errors.New("nil leader pending step")}
	}

	all := make([]*PendingStep, 0, 1+len(members))
	all = append(all, leader)
	all = append(all, members...)

	// Step 1a: Check for nil steps, foreign engines, duplicates, and dead/replayed
	// tokens BEFORE mutating any state.
	seenSteps := make(map[*PendingStep]struct{}, len(all))
	seenRuns := make(map[*ChainRun]struct{}, len(all))
	for _, p := range all {
		if p == nil || p.run == nil || p.run.engine != e {
			e.failWholeGroupIncompatible(all, errors.New("nil or foreign pending step in group"))
			return "", &FailedError{Reason: ReasonIncompatibleGroup, Cause: errors.New("nil or foreign pending step in group")}
		}
		if _, dup := seenSteps[p]; dup {
			e.failWholeGroupIncompatible(all, errors.New("duplicate pending step in group"))
			return "", &FailedError{Reason: ReasonIncompatibleGroup, Cause: errors.New("duplicate pending step in group")}
		}
		seenSteps[p] = struct{}{}
		if _, dupRun := seenRuns[p.run]; dupRun {
			e.failWholeGroupIncompatible(all, errors.New("duplicate run in group"))
			return "", &FailedError{Reason: ReasonIncompatibleGroup, Cause: errors.New("duplicate run in group")}
		}
		seenRuns[p.run] = struct{}{}
		if !p.isLive() {
			err := &FailedError{Reason: ReasonInvalidCompletion, Cause: errors.New("replayed or inactive pending step in group")}
			// O-1: make the replayed member's run terminal consistently with EvaluatePending.
			p.run.failInvalidCompletion(p, err)
			e.failWholeGroupIncompatible(all, err)
			return "", err
		}
	}

	// Claim all tokens atomically (O-2: if any fails concurrently, fail-closed
	// on ALL supplied steps so none stay live).
	claimed := make([]*PendingStep, 0, len(all))
	for _, p := range all {
		if err := p.claimForCompletion(); err != nil {
			for _, c := range claimed {
				c.run.completePending(c, StepOutcome{
					Index:   c.index,
					Dialect: c.dialect,
					Kind:    c.kind,
					Class:   StepClassFailed,
					Reason:  ReasonIncompatibleGroup,
					Domain:  DomainIO,
					Err:     err,
				}, nil, nil)
			}
			p.run.failInvalidCompletion(p, err)
			e.failWholeGroupIncompatible(all, err)
			return "", err
		}
		claimed = append(claimed, p)
	}

	// Step 1b: Validate group compatibility (A1 §3 rule 8 + v1 §3.3 safety floor)
	// and per-consumer dialect re-admission for every member BEFORE any I/O.
	leader.run.mu.Lock()
	leaderIO := leader.run.domains.IO
	leader.run.mu.Unlock()

	if leader.groupKey == "" {
		return e.failClaimedGroup(claimed, errors.New("empty GroupKey cannot be grouped"))
	}
	leaderDial, err := e.resolveDialect(leader.compiled.Consumer(), leader.compiled.Dialect())
	if err != nil {
		return e.failClaimedGroup(claimed, fmt.Errorf("leader dialect admission failed: %w", err))
	}

	inlineCtxs := make([]context.Context, 0, len(claimed))
	for _, m := range claimed {
		m.run.mu.Lock()
		mIO := m.run.domains.IO
		mInlineCtx := m.run.domains.Inline.Ctx
		m.run.mu.Unlock()
		inlineCtxs = append(inlineCtxs, mInlineCtx)

		if m.groupKey != leader.groupKey {
			return e.failClaimedGroup(claimed, errors.New("mismatched GroupKey"))
		}
		if mIO.Budget.Tracker != leaderIO.Budget.Tracker {
			return e.failClaimedGroup(claimed, errors.New("mismatched IO BudgetTracker"))
		}
		if mIO.Budget.MaxEvalTime != leaderIO.Budget.MaxEvalTime || mIO.Budget.CostLimit != leaderIO.Budget.CostLimit {
			return e.failClaimedGroup(claimed, errors.New("mismatched IO Budget ceiling"))
		}
		if !sameContextIdentity(mIO.Ctx, leaderIO.Ctx) {
			return e.failClaimedGroup(claimed, errors.New("mismatched IO Ctx identity"))
		}
		// R-1 fail-closed safety floor: semantic compiled identity + admitted-input hash.
		if !sameCompiledSemanticIdentity(m.compiled, leader.compiled) {
			return e.failClaimedGroup(claimed, errors.New("mismatched semantic compiled identity"))
		}
		if !leader.admittedInputValid || !m.admittedInputValid || m.admittedInputHash != leader.admittedInputHash {
			return e.failClaimedGroup(claimed, errors.New("mismatched or unhashable admitted input"))
		}
		if _, admErr := e.resolveDialect(m.compiled.Consumer(), m.compiled.Dialect()); admErr != nil {
			return e.failClaimedGroup(claimed, fmt.Errorf("member dialect admission failed: %w", admErr))
		}
	}

	groupID := fmt.Sprintf("grp-%d", e.groupSeq.Add(1))
	caps := leaderDial.Declares()

	// Step 2a: Cancel-at-barrier check (0 calls).
	if ctxErr := leaderIO.Ctx.Err(); ctxErr != nil {
		for _, m := range claimed {
			m.run.completePending(m, StepOutcome{
				Index:   m.index,
				Dialect: m.dialect,
				Kind:    m.kind,
				Class:   StepClassCanceled,
				Reason:  celengine.ReasonCanceled,
				GroupID: groupID,
				Domain:  DomainIO,
				Err:     &FailedError{Reason: celengine.ReasonCanceled, Cause: ctxErr},
			}, nil, nil)
		}
		return groupID, nil
	}

	// If every member's Inline.Ctx is already canceled (D2 cascade), cancel all with 0 calls.
	allInlineCanceled := true
	for _, ic := range inlineCtxs {
		if ic.Err() == nil {
			allInlineCanceled = false
			break
		}
	}
	if allInlineCanceled {
		ctxErr := inlineCtxs[0].Err()
		for _, m := range claimed {
			m.run.completePending(m, StepOutcome{
				Index:   m.index,
				Dialect: m.dialect,
				Kind:    m.kind,
				Class:   StepClassCanceled,
				Reason:  celengine.ReasonCanceled,
				GroupID: groupID,
				Domain:  DomainIO,
				Err:     &FailedError{Reason: celengine.ReasonCanceled, Cause: ctxErr},
			}, nil, nil)
		}
		return groupID, nil
	}

	// Step 2b: Exhausted IO tracker at dispatch → budget for all members, 0 calls.
	if leaderIO.Budget.Tracker != nil && leaderIO.Budget.Tracker.Exhausted() {
		for _, m := range claimed {
			m.run.completePending(m, StepOutcome{
				Index:   m.index,
				Dialect: m.dialect,
				Kind:    m.kind,
				Class:   StepClassBudget,
				Reason:  ReasonBudgetExhausted,
				GroupID: groupID,
				Domain:  DomainIO,
				Err:     ErrBudgetExhausted,
			}, nil, nil)
		}
		return groupID, nil
	}

	// Step 3 & 4: ONE ordinary evaluation on leader's compiled rule and request
	// under leaderIO.Ctx only, cascading to cancel the evaluation ONLY when ALL
	// members' Inline ctxs are done (R-2). Charges the shared IO tracker once.
	evalCtx, stopCascade := cascadeAllCanceledContext(leaderIO.Ctx, inlineCtxs)
	defer stopCascade()

	dec, elapsed, evalErr := e.evaluateCore(evalCtx, leader.compiled, leader.input, leaderIO.Budget)
	leaderOut := classifyDecisionToOutcome(leader.index, leader.dialect, leader.kind, DomainIO, groupID, caps, dec, elapsed, evalErr)
	leaderOut.GroupElapsed = elapsed

	// Step 5: Distribute outcome. R-2: any member whose own Inline ctx is done
	// records canceled (Elapsed: 0, GroupElapsed: 0, not charged) without
	// affecting live members. O-a: attribute the charged Elapsed to the first
	// live member (leader when live, or first live member when leader is
	// canceled) so tracker spent == Σ StepOutcome.Elapsed across the group.
	chargedAttributed := false
	for i, m := range claimed {
		if memberCtxErr := inlineCtxs[i].Err(); memberCtxErr != nil {
			out := StepOutcome{
				Index:        m.index,
				Dialect:      m.dialect,
				Kind:         m.kind,
				Class:        StepClassCanceled,
				Reason:       celengine.ReasonCanceled,
				Elapsed:      0,
				GroupElapsed: 0,
				GroupID:      groupID,
				Domain:       DomainIO,
				Err:          &FailedError{Reason: celengine.ReasonCanceled, Cause: memberCtxErr},
			}
			m.run.completePending(m, out, nil, nil)
			continue
		}
		out := cloneStepOutcome(leaderOut)
		out.Index = m.index
		if !chargedAttributed {
			out.Elapsed = elapsed
			chargedAttributed = true
		} else {
			out.Elapsed = 0
		}
		var detail any
		if m.kind == KindJudgment && out.Class == StepClassPass {
			detail = copyJudgmentDetail(dec.Detail, caps.CopyDetail)
		}
		m.run.completePending(m, out, detail, caps.CopyDetail)
	}
	return groupID, nil
}

func (e *Engine) failWholeGroupIncompatible(all []*PendingStep, cause error) {
	seen := make(map[*PendingStep]struct{}, len(all))
	for _, p := range all {
		if p == nil || p.run == nil || p.run.engine != e {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		if p.claimForCompletion() == nil {
			p.run.completePending(p, StepOutcome{
				Index:   p.index,
				Dialect: p.dialect,
				Kind:    p.kind,
				Class:   StepClassFailed,
				Reason:  ReasonIncompatibleGroup,
				Domain:  DomainIO,
				Err:     &FailedError{Reason: ReasonIncompatibleGroup, Cause: cause},
			}, nil, nil)
		}
	}
}

func (e *Engine) failClaimedGroup(claimed []*PendingStep, cause error) (string, error) {
	fErr := &FailedError{Reason: ReasonIncompatibleGroup, Cause: cause}
	for _, m := range claimed {
		m.run.completePending(m, StepOutcome{
			Index:   m.index,
			Dialect: m.dialect,
			Kind:    m.kind,
			Class:   StepClassFailed,
			Reason:  ReasonIncompatibleGroup,
			Domain:  DomainIO,
			Err:     fErr,
		}, nil, nil)
	}
	return "", fErr
}

// Resume consumes the engine-recorded outcome of the run's pending step, applies
// StepPolicy, records trace/judgments, and advances the run (v1 §3.1, §3.2).
func (r *ChainRun) Resume() (Control, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.state == chainStateEvaluating {
		return Control{}, errors.New("ruleengine: cannot call Resume while an evaluation is in progress")
	}
	if r.state == chainStatePending {
		// Terminal completion after expiry/cancel (§3.2 point 9, A1 D2): if a
		// pending step was never evaluated and the run/IO context is already
		// canceled, complete it terminally as canceled so no pending step is
		// ever left uncompleted. r.mu already guards p.consumed (RV2-1).
		if ctxErr := firstCtxErr(r.domains.Inline.Ctx, r.domains.IO.Ctx); ctxErr != nil {
			p := r.pending
			p.consumed = true
			r.pendingOutcome = StepOutcome{
				Index:   p.index,
				Dialect: p.dialect,
				Kind:    p.kind,
				Class:   StepClassCanceled,
				Reason:  celengine.ReasonCanceled,
				Domain:  DomainIO,
				Err:     &FailedError{Reason: celengine.ReasonCanceled, Cause: ctxErr},
			}
			r.pendingDetail = nil
			r.state = chainStatePendingCompleted
		} else {
			return Control{}, errors.New("ruleengine: pending step has not been evaluated yet")
		}
	}
	if r.state != chainStatePendingCompleted {
		return Control{}, errors.New("ruleengine: no completed pending step to resume")
	}

	out := r.pendingOutcome
	detail := r.pendingDetail
	copier := r.pendingCopier
	r.pending = nil
	r.pendingOutcome = StepOutcome{}
	r.pendingDetail = nil
	r.pendingCopier = nil

	ctrl := r.policy(cloneStepOutcome(out))
	if out.Class == StepClassBudget || out.Class == StepClassCanceled || out.Reason == ReasonInvalidCompletion {
		ctrl.Stop = true
	}
	r.recordStepLocked(out, ctrl, detail, copier)
	r.nextPos++
	if ctrl.Stop || r.nextPos >= len(r.elements) {
		r.state = chainStateDone
	} else {
		r.state = chainStateReady
	}
	return ctrl, nil
}

// Judgments returns an immutable, defensive-copy JudgmentView of all judgments
// recorded so far on this run. Raw judgments are never serialized into ChainTrace.
func (r *ChainRun) Judgments() JudgmentView {
	r.mu.Lock()
	defer r.mu.Unlock()
	jm := make(map[int]storedJudgment, len(r.judgments))
	steps := make([]int, 0, len(r.judgments))
	for s, sj := range r.judgments {
		jm[s] = storedJudgment{
			judgment: Judgment{
				Step:    sj.judgment.Step,
				Dialect: sj.judgment.Dialect,
				Detail:  copyJudgmentDetail(sj.judgment.Detail, sj.copier),
			},
			copier: sj.copier,
		}
		steps = append(steps, s)
	}
	sort.Ints(steps)
	return &stepJudgmentView{judgments: jm, steps: steps}
}

// Result returns the ChainResult and structured in-memory ChainTrace for the
// run. Note (O-d): ChainLimits.MaxTraceBytes is enforced at serialization time
// when consumers pass the returned ChainTrace to EncodeChainTrace (which
// validates all closed tokens and applies deterministic truncation); Result()
// itself preserves the structured in-memory trace without byte truncation.
func (r *ChainRun) Result() (ChainResult, ChainTrace) {
	r.mu.Lock()
	defer r.mu.Unlock()

	stepsCopy := make([]StepOutcome, len(r.steps))
	for i, s := range r.steps {
		stepsCopy[i] = cloneStepOutcome(s)
	}
	traceElems := make([]ChainTraceElement, len(r.trace))
	for i, te := range r.trace {
		te.Provenance = cloneProvenanceMap(te.Provenance)
		traceElems[i] = te
	}

	var unexec []int
	if r.nextPos < len(r.elements) {
		unexec = make([]int, 0, len(r.elements)-r.nextPos)
		for _, spec := range r.elements[r.nextPos:] {
			unexec = append(unexec, spec.elem.Index)
		}
	}

	res := ChainResult{
		Through:    len(r.steps),
		Stopped:    r.stopped,
		Verdict:    r.verdict,
		Deciding:   cloneStepOutcome(r.deciding),
		Steps:      stepsCopy,
		Unexecuted: append([]int(nil), unexec...),
	}

	ct := ChainTrace{
		Elements:   traceElems,
		Unexecuted: unexec,
	}
	inlineTr := r.domains.Inline.Budget.Tracker
	ioTr := r.domains.IO.Budget.Tracker
	if inlineTr != nil && inlineTr == ioTr {
		ms := max(inlineTr.Remaining().Milliseconds(), int64(0))
		ct.BudgetRemainingMs = &ms
	} else {
		if inlineTr != nil {
			ms := max(inlineTr.Remaining().Milliseconds(), int64(0))
			ct.InlineBudgetRemainingMs = &ms
		}
		if ioTr != nil {
			ms := max(ioTr.Remaining().Milliseconds(), int64(0))
			ct.IOBudgetRemainingMs = &ms
		}
	}
	return res, ct
}

func (p *PendingStep) isLive() bool {
	if p == nil || p.run == nil {
		return false
	}
	p.run.mu.Lock()
	defer p.run.mu.Unlock()
	return !p.consumed && p.run.state == chainStatePending && p.run.pending == p
}

func (p *PendingStep) claimForCompletion() error {
	if p == nil || p.run == nil {
		return &FailedError{Reason: ReasonInvalidCompletion, Cause: errors.New("nil pending step")}
	}
	p.run.mu.Lock()
	defer p.run.mu.Unlock()
	if p.consumed {
		return &FailedError{Reason: ReasonInvalidCompletion, Cause: errors.New("completion token already used")}
	}
	if p.run.state != chainStatePending || p.run.pending != p {
		return &FailedError{Reason: ReasonInvalidCompletion, Cause: errors.New("pending step not active for run")}
	}
	p.consumed = true
	return nil
}

func (r *ChainRun) completePending(p *PendingStep, out StepOutcome, detail any, copier func(any) any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending != p || r.state != chainStatePending {
		return
	}
	r.pendingOutcome = out
	r.pendingDetail = detail
	r.pendingCopier = copier
	r.state = chainStatePendingCompleted
}

func (r *ChainRun) failInvalidCompletion(p *PendingStep, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == chainStateDone {
		return
	}
	// O-1: if a legitimate completion outcome was already recorded (before
	// Resume), record it first so terminal outcomes are never suppressed by
	// a subsequent replay rejection (§3.7 item 7).
	if r.state == chainStatePendingCompleted {
		r.recordStepLocked(r.pendingOutcome, Control{Stop: true}, r.pendingDetail, r.pendingCopier)
		r.pendingOutcome = StepOutcome{}
		r.pendingDetail = nil
		r.pendingCopier = nil
	}
	// N-a: record the invalid completion against the currently pending step's
	// index when one is active (so a stale token from step 1 replayed during
	// step 2 fails step 2 rather than duplicating step 1).
	target := p
	if r.pending != nil {
		target = r.pending
		r.pending.consumed = true
	}
	out := StepOutcome{
		Index:   target.index,
		Dialect: target.dialect,
		Kind:    target.kind,
		Class:   StepClassFailed,
		Reason:  ReasonInvalidCompletion,
		Domain:  DomainIO,
		Err:     err,
	}
	r.recordStepLocked(out, Control{Stop: true}, nil, nil)
	if r.state == chainStatePending && r.nextPos < len(r.elements) {
		r.nextPos++
	}
	r.pending = nil
	r.state = chainStateDone
}

func (r *ChainRun) recordStepLocked(out StepOutcome, ctrl Control, detail any, copier func(any) any) {
	if out.Kind == KindJudgment && out.Class == StepClassPass {
		r.judgments[out.Index] = storedJudgment{
			judgment: Judgment{
				Step:    out.Index,
				Dialect: out.Dialect,
				Detail:  copyJudgmentDetail(detail, copier),
			},
			copier: copier,
		}
	}
	r.steps = append(r.steps, cloneStepOutcome(out))
	r.trace = append(r.trace, ChainTraceElement{
		Index:      out.Index,
		Dialect:    out.Dialect,
		Kind:       out.Kind,
		Class:      out.Class,
		Reason:     out.Reason,
		Verdict:    ctrl.Verdict,
		DurationMs: max(out.Elapsed.Milliseconds(), int64(0)),
		GroupID:    out.GroupID,
		Domain:     out.Domain,
		Provenance: cloneProvenanceMap(out.Provenance),
	})
	r.deciding = cloneStepOutcome(out)
	if ctrl.Stop {
		r.stopped = true
		r.verdict = ctrl.Verdict
	} else if ctrl.Verdict != "" {
		r.verdict = ctrl.Verdict
	}
}

type stepJudgmentView struct {
	judgments map[int]storedJudgment
	steps     []int
}

func (v *stepJudgmentView) Get(step int) (Judgment, bool) {
	sj, ok := v.judgments[step]
	if !ok {
		return Judgment{}, false
	}
	return Judgment{
		Step:    sj.judgment.Step,
		Dialect: sj.judgment.Dialect,
		Detail:  copyJudgmentDetail(sj.judgment.Detail, sj.copier),
	}, true
}

func (v *stepJudgmentView) Steps() []int {
	return append([]int(nil), v.steps...)
}

func (r *ChainRun) buildJudgmentViewLocked(currentStep int) JudgmentView {
	jm := make(map[int]storedJudgment)
	var steps []int
	for s, sj := range r.judgments {
		if s < currentStep {
			jm[s] = storedJudgment{
				judgment: Judgment{
					Step:    sj.judgment.Step,
					Dialect: sj.judgment.Dialect,
					Detail:  copyJudgmentDetail(sj.judgment.Detail, sj.copier),
				},
				copier: sj.copier,
			}
			steps = append(steps, s)
		}
	}
	sort.Ints(steps)
	return &stepJudgmentView{judgments: jm, steps: steps}
}

// copyJudgmentDetail returns an independent copy of detail using the
// dialect-declared concrete-type-preserving CopyDetail copier (R-3 option b).
func copyJudgmentDetail(detail any, copier func(any) any) any {
	if detail == nil || copier == nil {
		return nil
	}
	return copier(detail)
}

func (r *ChainRun) resolveStepInputLocked(stepIndex int) (Input, error) {
	// O-5: base Activation was defensively copied once at construction and is
	// shared read-only across steps; only allocate a fresh top-level map when
	// the Enricher supplies additions.
	if r.enricher == nil {
		return Input{
			Activation:       r.base.Activation,
			Payload:          r.base.Payload,
			AdmittedInputKey: r.base.AdmittedInputKey,
		}, nil
	}

	view := r.buildJudgmentViewLocked(stepIndex)
	additions, err := r.enricher.Additions(stepIndex, view)
	if err != nil {
		return Input{}, fmt.Errorf("ruleengine: enricher error: %w", err)
	}
	if len(additions) == 0 {
		return Input{
			Activation:       r.base.Activation,
			Payload:          r.base.Payload,
			AdmittedInputKey: r.base.AdmittedInputKey,
		}, nil
	}

	act := make(map[string]any, len(r.base.Activation)+len(additions))
	maps.Copy(act, r.base.Activation)

	var chainNS map[string]any
	for k, v := range additions {
		if k == "chain" {
			if _, exists := r.base.Activation["chain"]; exists {
				return Input{}, errors.New("ruleengine: enricher collides with base key \"chain\"")
			}
			m, ok := v.(map[string]any)
			if !ok {
				return Input{}, fmt.Errorf("ruleengine: enricher \"chain\" value must be map[string]any, got %T", v)
			}
			if chainNS == nil {
				chainNS = make(map[string]any, len(m))
			}
			for ck, cv := range m {
				chainNS[ck] = deepCopyValue(cv)
			}
			continue
		}
		if sub, isChainPrefixed := strings.CutPrefix(k, "chain."); isChainPrefixed {
			if sub == "" {
				return Input{}, errors.New("ruleengine: empty key under chain namespace")
			}
			if _, exists := r.base.Activation["chain"]; exists {
				return Input{}, errors.New("ruleengine: enricher collides with base key \"chain\"")
			}
			if chainNS == nil {
				chainNS = make(map[string]any)
			}
			chainNS[sub] = deepCopyValue(v)
			continue
		}
		if _, isReserved := r.reserved[k]; isReserved {
			if _, exists := r.base.Activation[k]; exists {
				return Input{}, fmt.Errorf("ruleengine: enricher collides with base key %q", k)
			}
			if err := validateReservedAdditionType(k, v); err != nil {
				return Input{}, err
			}
			act[k] = deepCopyValue(v)
			continue
		}
		return Input{}, fmt.Errorf("ruleengine: enricher key %q is outside chain.* and not a declared reserved name", k)
	}
	if chainNS != nil {
		act["chain"] = chainNS
	}
	return Input{
		Activation:       act,
		Payload:          r.base.Payload,
		AdmittedInputKey: r.base.AdmittedInputKey,
	}, nil
}

// validateReservedAdditionType enforces Amendment A1 §5 D4 type conformance on
// reserved judgment-carry additions.
// 2a: replace hard-coded legacy messagepolicy reserved-name type checks with
// consumer-declared validators (e.g. map[string]func(any) error) at the next
// API touch with System One (O-c).
func validateReservedAdditionType(name string, val any) error {
	switch name {
	case "judgment_flags":
		if _, ok := val.(map[string]bool); !ok {
			return fmt.Errorf("ruleengine: reserved key %q requires map[string]bool, got %T", name, val)
		}
		return nil
	case "judgment_labels":
		if _, ok := val.(map[string]string); !ok {
			return fmt.Errorf("ruleengine: reserved key %q requires map[string]string, got %T", name, val)
		}
		return nil
	case "judgment_scores":
		if _, ok := val.(map[string]float64); !ok {
			return fmt.Errorf("ruleengine: reserved key %q requires map[string]float64, got %T", name, val)
		}
		return nil
	default:
		switch val.(type) {
		case map[string]bool, map[string]string, map[string]float64, map[string]any:
			return nil
		default:
			return fmt.Errorf("ruleengine: reserved key %q requires a map value, got %T", name, val)
		}
	}
}

func classifyDecisionToOutcome(
	index int,
	dialect string,
	kind Kind,
	dom ChainDomainName,
	groupID string,
	caps Capabilities,
	dec Decision,
	elapsed time.Duration,
	err error,
) StepOutcome {
	var rawProv map[string]any
	if dec.Provenance != nil {
		rawProv = dec.Provenance.Fields
	}
	prov := filterValidProvenance(rawProv, caps)
	if err != nil {
		var sce *sourceCompileError
		if errors.As(err, &sce) {
			if errors.Is(sce.cause, errSourceCompileBudgetExhausted) {
				return StepOutcome{
					Index:      index,
					Dialect:    dialect,
					Kind:       kind,
					Class:      StepClassBudget,
					Reason:     ReasonCompileBudgetExhausted,
					Elapsed:    elapsed,
					GroupID:    groupID,
					Domain:     dom,
					Provenance: prov,
					RawTrace:   dec.Trace,
					Err:        sce.cause,
				}
			}
			return StepOutcome{
				Index:      index,
				Dialect:    dialect,
				Kind:       kind,
				Class:      StepClassFailed,
				Reason:     ReasonCompileError,
				Elapsed:    elapsed,
				GroupID:    groupID,
				Domain:     dom,
				Provenance: prov,
				RawTrace:   dec.Trace,
				Err:        sce.cause,
			}
		}

		class, reason := classifyEvalErr(err, caps)
		return StepOutcome{
			Index:      index,
			Dialect:    dialect,
			Kind:       kind,
			Class:      class,
			Reason:     reason,
			Elapsed:    elapsed,
			GroupID:    groupID,
			Domain:     dom,
			Provenance: prov,
			RawTrace:   dec.Trace,
			Err:        err,
		}
	}

	if kind == KindFilter && !dec.Bool {
		return StepOutcome{
			Index:      index,
			Dialect:    dialect,
			Kind:       kind,
			Class:      StepClassFail,
			Elapsed:    elapsed,
			GroupID:    groupID,
			Domain:     dom,
			Provenance: prov,
			RawTrace:   dec.Trace,
		}
	}

	return StepOutcome{
		Index:      index,
		Dialect:    dialect,
		Kind:       kind,
		Class:      StepClassPass,
		Elapsed:    elapsed,
		GroupID:    groupID,
		Domain:     dom,
		Provenance: prov,
		RawTrace:   dec.Trace,
	}
}

func classifyEvalErr(err error, caps Capabilities) (StepClass, string) {
	if errors.Is(err, ErrBudgetExhausted) {
		return StepClassBudget, ReasonBudgetExhausted
	}
	var unc *UncertainError
	if errors.As(err, &unc) {
		return StepClassUncertain, sanitizeStepReason(unc.Reason, StepClassUncertain, caps)
	}
	var fErr *FailedError
	if errors.As(err, &fErr) {
		if fErr.Reason == celengine.ReasonCanceled {
			return StepClassCanceled, celengine.ReasonCanceled
		}
		return StepClassFailed, sanitizeStepReason(fErr.Reason, StepClassFailed, caps)
	}
	var evalErr *celengine.EvalError
	if errors.As(err, &evalErr) {
		if evalErr.Reason == celengine.ReasonCanceled {
			return StepClassCanceled, celengine.ReasonCanceled
		}
		return StepClassFailed, sanitizeStepReason(evalErr.Reason, StepClassFailed, caps)
	}
	var uErr *UnsupportedDialectError
	if errors.As(err, &uErr) {
		return StepClassFailed, sanitizeStepReason(uErr.Reason, StepClassFailed, caps)
	}
	if errors.Is(err, context.Canceled) {
		return StepClassCanceled, celengine.ReasonCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return StepClassFailed, celengine.ReasonTimeout
	}
	return StepClassFailed, ReasonEvalFailed
}

func classifyErrorReason(err error, class StepClass, caps Capabilities) string {
	_, reason := classifyEvalErr(err, caps)
	if reason == "" {
		return sanitizeStepReason("", class, caps)
	}
	return reason
}

func cloneStepOutcome(out StepOutcome) StepOutcome {
	out.Provenance = cloneProvenanceMap(out.Provenance)
	return out
}

// sameCompiledSemanticIdentity implements the R-1 fail-closed safety floor for
// grouped evaluations: consumer, dialect, and kind must match, and either both
// handles declare the same non-empty ProgramID or (with no declared ProgramID)
// both carry the exact same non-nil rep pointer. Opaque reps are never JSON-hashed.
func sameCompiledSemanticIdentity(a, b Compiled) bool {
	if a.Consumer() != b.Consumer() || a.Dialect() != b.Dialect() || a.Kind() != b.Kind() {
		return false
	}
	if a.programID != "" || b.programID != "" {
		return a.programID != "" && a.programID == b.programID
	}
	if a.rep == nil || b.rep == nil {
		return false
	}
	rvA, rvB := reflect.ValueOf(a.rep), reflect.ValueOf(b.rep)
	return rvA.Kind() == reflect.Pointer && rvB.Kind() == reflect.Pointer &&
		rvA.Type() == rvB.Type() && rvA.Pointer() == rvB.Pointer()
}

// computeAdmittedInputHash implements the R-1 fail-closed admitted-input hash
// (O-3: uses Input.AdmittedInputKey when present; otherwise JSON-marshals
// Activation and Payload and fails closed on any marshal error).
func computeAdmittedInputHash(in Input) (string, error) {
	if in.AdmittedInputKey != "" {
		h := sha256.Sum256([]byte("key:" + in.AdmittedInputKey))
		return hex.EncodeToString(h[:]), nil
	}
	h := sha256.New()
	if in.Activation != nil {
		raw, err := json.Marshal(in.Activation)
		if err != nil {
			return "", fmt.Errorf("ruleengine: failed to marshal activation for admitted-input hash: %w", err)
		}
		h.Write([]byte("act:"))
		h.Write(raw)
	}
	if in.Payload != nil {
		raw, err := json.Marshal(in.Payload)
		if err != nil {
			return "", fmt.Errorf("ruleengine: failed to marshal payload for admitted-input hash: %w", err)
		}
		h.Write([]byte("|pay:"))
		h.Write(raw)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func sameContextIdentity(a, b context.Context) bool {
	if a == nil || b == nil {
		return a == b
	}
	ta, tb := reflect.TypeOf(a), reflect.TypeOf(b)
	if ta != tb || !ta.Comparable() {
		return false
	}
	return a == b
}

func firstCtxErr(ctxs ...context.Context) error {
	for _, c := range ctxs {
		if c != nil {
			if err := c.Err(); err != nil {
				return err
			}
		}
	}
	return nil
}

// cascadeCancelContext returns a context derived from primary that is also
// canceled immediately if any watchCtx finishes (D2 engine-observed cascade
// for single-run EvaluatePending). Uses context.AfterFunc so zero goroutines
// are leaked.
func cascadeCancelContext(primary context.Context, watchCtxs ...context.Context) (context.Context, func()) {
	var unique []context.Context
	for _, wc := range watchCtxs {
		if wc == nil || sameContextIdentity(wc, primary) {
			continue
		}
		if wc.Done() == nil {
			continue
		}
		dup := false
		for _, u := range unique {
			if sameContextIdentity(u, wc) {
				dup = true
				break
			}
		}
		if !dup {
			unique = append(unique, wc)
		}
	}
	if len(unique) == 0 {
		return primary, func() {}
	}
	ctx, cancel := context.WithCancelCause(primary)
	stops := make([]func() bool, 0, len(unique))
	for _, wc := range unique {
		wc := wc
		stops = append(stops, context.AfterFunc(wc, func() {
			cancel(context.Cause(wc))
		}))
	}
	return ctx, func() {
		for _, s := range stops {
			s()
		}
		cancel(nil)
	}
}

// cascadeAllCanceledContext returns a context derived from primary that cancels
// only when ALL watchCtxs have finished (R-2: one member's Inline-ctx
// cancellation must never cancel a grouped evaluation while another member is
// still live). Uses context.AfterFunc so zero goroutines are leaked.
func cascadeAllCanceledContext(primary context.Context, watchCtxs []context.Context) (context.Context, func()) {
	var live []context.Context
	for _, wc := range watchCtxs {
		if wc == nil || wc.Done() == nil {
			// A non-cancelable member ctx (e.g. context.Background()) means
			// "all members canceled" can never become true.
			return primary, func() {}
		}
		if wc.Err() != nil {
			continue
		}
		dup := false
		for _, u := range live {
			if sameContextIdentity(u, wc) {
				dup = true
				break
			}
		}
		if !dup {
			live = append(live, wc)
		}
	}
	if len(live) == 0 {
		return primary, func() {}
	}
	ctx, cancel := context.WithCancelCause(primary)
	var remaining atomic.Int32
	remaining.Store(int32(len(live)))
	stops := make([]func() bool, 0, len(live))
	for _, wc := range live {
		wc := wc
		stops = append(stops, context.AfterFunc(wc, func() {
			if remaining.Add(-1) == 0 {
				cancel(context.Cause(wc))
			}
		}))
	}
	return ctx, func() {
		for _, s := range stops {
			s()
		}
		cancel(nil)
	}
}

func deepCopyActivationMap(m map[string]any) (map[string]any, error) {
	if m == nil {
		return nil, nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		cp, err := deepCopyActivationValue(v)
		if err != nil {
			return nil, fmt.Errorf("key %q: %w", k, err)
		}
		out[k] = cp
	}
	return out, nil
}

// deepCopyActivationValue copies JSON-shaped primitives, time.Time/time.Duration,
// and standard map/slice containers, rejecting any non-copyable container,
// pointer, or struct type so base.Activation never aliases caller state (O-b).
func deepCopyActivationValue(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch x := v.(type) {
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, string, time.Time, time.Duration:
		return x, nil
	case map[string]any:
		return deepCopyActivationMap(x)
	case map[string]bool:
		cp := make(map[string]bool, len(x))
		maps.Copy(cp, x)
		return cp, nil
	case map[string]string:
		cp := make(map[string]string, len(x))
		maps.Copy(cp, x)
		return cp, nil
	case map[string]float64:
		cp := make(map[string]float64, len(x))
		maps.Copy(cp, x)
		return cp, nil
	case map[string][]string:
		cp := make(map[string][]string, len(x))
		for k, vals := range x {
			cp[k] = slices.Clone(vals)
		}
		return cp, nil
	case []any:
		cp := make([]any, len(x))
		for i, elem := range x {
			ce, err := deepCopyActivationValue(elem)
			if err != nil {
				return nil, fmt.Errorf("index %d: %w", i, err)
			}
			cp[i] = ce
		}
		return cp, nil
	case []string:
		return slices.Clone(x), nil
	case []int:
		return slices.Clone(x), nil
	case []bool:
		return slices.Clone(x), nil
	case []float64:
		return slices.Clone(x), nil
	case []byte:
		return slices.Clone(x), nil
	default:
		return nil, fmt.Errorf("unsupported non-copyable activation value type %T", v)
	}
}

func deepCopyMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = deepCopyValue(v)
	}
	return out
}

// deepCopyValue copies standard activation container types (maps and slices)
// without reflection or duck-typed Clone() conventions (R-3 option b, O-3).
func deepCopyValue(v any) any {
	if v == nil {
		return nil
	}
	switch x := v.(type) {
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, string, time.Time, time.Duration:
		return x
	case map[string]any:
		return deepCopyMap(x)
	case map[string]bool:
		cp := make(map[string]bool, len(x))
		maps.Copy(cp, x)
		return cp
	case map[string]string:
		cp := make(map[string]string, len(x))
		maps.Copy(cp, x)
		return cp
	case map[string]float64:
		cp := make(map[string]float64, len(x))
		maps.Copy(cp, x)
		return cp
	case map[string][]string:
		cp := make(map[string][]string, len(x))
		for k, vals := range x {
			cp[k] = slices.Clone(vals)
		}
		return cp
	case []any:
		cp := make([]any, len(x))
		for i, elem := range x {
			cp[i] = deepCopyValue(elem)
		}
		return cp
	case []string:
		return slices.Clone(x)
	case []int:
		return slices.Clone(x)
	case []bool:
		return slices.Clone(x)
	case []float64:
		return slices.Clone(x)
	case []byte:
		return slices.Clone(x)
	default:
		return v
	}
}
