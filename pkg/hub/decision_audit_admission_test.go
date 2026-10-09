// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/auditevent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Direct emission is a finite returned-Decision boundary fixture, never a
// production mapper/admission. It requires known nonempty PermissionID and
// trusted correlation; missing classes remain a #2379 placement obligation.
// No Decide/Store inputs, backend ingestion, durability or zero routine
// CreateDecisionAudit claim follows from these synchronous local observations.
type directAuditMode uint8

const (
	directAuditAccept directAuditMode = iota
	directAuditDisabled
	directAuditHandlerError
	directAuditHandlerPanic
	directAuditWriterError
	directAuditWriterPanic
	directAuditShortWrite
	directAuditZeroWrite
	directAuditLocalCancel
	directAuditCallerCancel
	directAuditTimer
	directAuditCompleteAt
	directAuditCompleteLate
	directAuditControlled
	directAuditActiveClose
)

type directDecisionAuditCaller = auditFixtureCaller

type directDecisionAuditWriter struct {
	mode            directAuditMode
	calls, complete int
	data            []byte
}

func (w *directDecisionAuditWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls > 8 || len(p) == 0 || len(p) > decisionAuditRecordMaxBytes {
		return 0, errors.New("finite direct writer bound")
	}
	switch w.mode {
	case directAuditWriterError:
		return 0, errors.New("finite direct write failure")
	case directAuditWriterPanic:
		panic("finite direct write panic")
	case directAuditShortWrite:
		return len(p) - 1, nil
	case directAuditZeroWrite:
		return 0, nil
	}
	w.data = append(w.data[:0], p...)
	w.complete++
	return len(p), nil
}

type directDecisionAuditHandler struct {
	mode                       directAuditMode
	writer                     *directDecisionAuditWriter
	clock                      *auditFixtureClock
	caller                     *directDecisionAuditCaller
	enabled, handled           int
	ctx                        context.Context
	record                     slog.Record
	ready, release, cancelSeen chan struct{}
	boundaryOK, gateFree       bool
	router                     *decisionAuditRouter
}

func (h *directDecisionAuditHandler) Enabled(ctx context.Context, _ slog.Level) bool {
	h.enabled++
	h.ctx = ctx
	return h.mode != directAuditDisabled
}

// Portable fixed slog kinds only: no LogValuer, formatter or arbitrary Any.
func directAuditAttrValue(v slog.Value, depth int) (any, error) {
	if depth > 4 {
		return nil, errors.New("finite direct group depth")
	}
	switch v.Kind() {
	case slog.KindString:
		return v.String(), nil
	case slog.KindInt64:
		return v.Int64(), nil
	case slog.KindBool:
		return v.Bool(), nil
	case slog.KindGroup:
		if len(v.Group()) > 16 {
			return nil, errors.New("finite direct group width")
		}
		m := make(map[string]any, len(v.Group()))
		for _, a := range v.Group() {
			value, err := directAuditAttrValue(a.Value, depth+1)
			if err != nil {
				return nil, err
			}
			if _, exists := m[a.Key]; exists {
				return nil, errors.New("duplicate direct attribute")
			}
			m[a.Key] = value
		}
		return m, nil
	default:
		return nil, errors.New("unsupported direct attribute")
	}
}

func (h *directDecisionAuditHandler) Handle(ctx context.Context, record slog.Record) error {
	h.handled++
	if h.handled > 8 || ctx != h.ctx || record.NumAttrs() > 24 {
		return errors.New("finite direct handler bound")
	}
	h.record = record.Clone()
	if h.mode == directAuditHandlerPanic {
		panic("finite direct handler panic")
	}
	if h.mode == directAuditHandlerError {
		return errors.New("finite direct handler failure")
	}
	switch h.mode {
	case directAuditLocalCancel:
		h.router.slot.cancel()
	case directAuditCallerCancel:
		h.caller.Cancel()
	case directAuditTimer:
		h.boundaryOK = h.clock.timerAt == h.clock.tick+decisionAuditCancelBudget
		h.clock.advance(h.clock.timerAt - time.Nanosecond)
		h.boundaryOK = h.boundaryOK && ctx.Err() == nil
		h.clock.advance(h.clock.timerAt)
		h.boundaryOK = h.boundaryOK && ctx.Err() == context.Canceled
	case directAuditCompleteAt, directAuditCompleteLate:
		// Isolate completion reading from the separately tested timer dispatch.
		h.clock.tick += decisionAuditCompleteBudget
		if h.mode == directAuditCompleteLate {
			h.clock.tick += time.Nanosecond
		}
	case directAuditControlled, directAuditActiveClose:
		close(h.ready)
		if h.mode == directAuditActiveClose {
			auditFixtureWait(ctx.Done())
			h.gateFree = h.router.gate.TryLock()
			if h.gateFree {
				h.router.gate.Unlock()
			}
			close(h.cancelSeen)
		}
		auditFixtureWait(h.release)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m := make(map[string]any, record.NumAttrs())
	var attrErr error
	record.Attrs(func(a slog.Attr) bool {
		value, err := directAuditAttrValue(a.Value, 0)
		if err != nil {
			attrErr = err
			return false
		}
		if _, exists := m[a.Key]; exists {
			attrErr = errors.New("duplicate direct attribute")
			return false
		}
		m[a.Key] = value
		return true
	})
	if attrErr != nil {
		return attrErr
	}
	data, err := json.Marshal(m)
	if err != nil {
		return errors.New("finite direct encoding failure")
	}
	n, err := h.writer.Write(data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	return nil
}
func (*directDecisionAuditHandler) WithAttrs([]slog.Attr) slog.Handler {
	panic("direct attributes forbidden")
}
func (*directDecisionAuditHandler) WithGroup(string) slog.Handler {
	panic("direct group decorator forbidden")
}

func buildDirectDecisionEnvelope(record *store.DecisionAuditRecord, eventID string) (auditevent.EnvelopeV1, error) {
	if record == nil || !isKnownPermission(record.PermissionID) ||
		record.PrincipalKind != "user" || (record.ResourceType != "project" && record.ResourceType != "agent") ||
		record.CredentialID != "" || record.CredentialType != "" ||
		record.CredentialName != "" || record.CredentialBoundaryKind != "" ||
		record.CredentialBoundaryProjectID != "" || record.CredentialLabels != "" ||
		record.ExecutorKind != "" || record.ExecutorID != "" {
		return auditevent.EnvelopeV1{}, errors.New("outside finite direct decision domain")
	}
	outcome, severity := auditevent.OutcomeAllow, auditevent.SeverityInfo
	switch record.Result {
	case "allow":
	case "deny":
		outcome, severity = auditevent.OutcomeDeny, auditevent.SeverityWarning
	default:
		return auditevent.EnvelopeV1{}, errors.New("unsupported direct outcome")
	}
	event := auditevent.EnvelopeV1{
		SchemaVersion: auditevent.SchemaVersion, EventID: eventID, OccurredAt: record.Timestamp.UTC(),
		Family: "authorization", Action: "decide", Phase: auditevent.PhaseDecision,
		Outcome: outcome, Severity: severity, CorrelationID: record.CorrelationID,
		Request:   &auditevent.RequestRef{ID: record.CorrelationID, Route: record.Route},
		Principal: &auditevent.IdentityRef{Kind: auditevent.IdentityUser, ID: record.PrincipalID},
		Resource:  &auditevent.ResourceRef{Kind: record.ResourceType, ID: record.ResourceID, Scope: auditevent.ResourceScopeSystem},
		Payload: auditevent.AuthorizationDecisionPayload{
			PermissionID: record.PermissionID, Permission: record.Permission, Reason: record.Reason,
			DeniedBy: record.DeniedBy, Sampled: strconv.FormatBool(record.Sampled),
		},
	}
	if err := auditevent.Validate(event); err != nil {
		return auditevent.EnvelopeV1{}, err
	}
	return event, nil
}

type directDecisionAuditAdapter struct {
	sink            *auditevent.SlogSink
	selected, decoy *directDecisionAuditHandler
	writer          *directDecisionAuditWriter
	caller          context.Context
	calls, emits    int
	ctx             context.Context
	record          store.DecisionAuditRecord
	event           auditevent.EnvelopeV1
	slot            *decisionAuditNewSlot
	router          *decisionAuditRouter
}

func (a *directDecisionAuditAdapter) AcceptDecision(ctx context.Context, record *store.DecisionAuditRecord) (decisionAuditAcceptance, error) {
	a.calls++
	if a.calls > 8 {
		return decisionAuditAcceptance{}, errors.New("finite direct invocation bound")
	}
	a.ctx, a.record, a.slot = ctx, *record, a.router.slot
	if ctx == a.caller || a.slot.caller != a.caller ||
		logging.RequestMetaFromContext(ctx) != nil || perfTraceFrom(ctx) != nil || routeFromContext(ctx) != "" {
		return decisionAuditAcceptance{}, errors.New("direct local context mismatch")
	}
	eventID := fmt.Sprintf("22222222-2222-4222-8222-%012d", a.calls)
	event, err := buildDirectDecisionEnvelope(record, eventID)
	if err != nil {
		return decisionAuditAcceptance{}, err
	}
	a.event = event
	if a.selected.writer != a.writer {
		return decisionAuditAcceptance{}, errors.New("direct writer binding mismatch")
	}
	enabled, handled, written, complete := a.selected.enabled, a.selected.handled, a.writer.calls, a.writer.complete
	decoyEnabled, decoyHandled, decoyWritten := a.decoy.enabled, a.decoy.handled, a.decoy.writer.calls
	a.emits++
	if err := a.sink.Emit(ctx, event); err != nil {
		return decisionAuditAcceptance{}, err
	}
	accepted := a.selected.enabled == enabled+1 && a.selected.handled == handled+1 &&
		a.writer.calls == written+1 && a.writer.complete == complete+1 &&
		a.decoy.enabled == decoyEnabled && a.decoy.handled == decoyHandled && a.decoy.writer.calls == decoyWritten &&
		a.selected.ctx == ctx && ctx.Err() == nil
	return decisionAuditAcceptance{accepted: accepted}, nil
}

type directDecisionAuditSettings struct {
	*fakeHubSettingStore
	calls int
	fail  bool
}

func (s *directDecisionAuditSettings) ListHubSettings(ctx context.Context) ([]store.HubSetting, error) {
	s.calls++
	if s.calls > 8 || s.fail {
		return nil, errors.New("finite direct settings failure")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := s.fakeHubSettingStore.ListHubSettings(ctx)
	if len(rows) > 1 {
		return nil, errors.New("finite direct settings row bound")
	}
	return rows, err
}

type directDecisionAuditFixture struct {
	router                    *decisionAuditRouter
	adapter                   *directDecisionAuditAdapter
	a, b, selected, decoy     *directDecisionAuditHandler
	loggerA, loggerB          *slog.Logger
	writerA, writerB          *directDecisionAuditWriter
	clock                     *auditFixtureClock
	legacy                    *auditFixtureLegacy
	base                      *directDecisionAuditCaller
	meta                      *logging.RequestMeta
	installed, routed, caller context.Context
	trace                     *PerfTrace
	ops                       *OperationalSettings
	settings                  *directDecisionAuditSettings
	contract                  decisionAuditLocalContract
}

// Independent literal role census on each side, with separately constructed live
// bindings. No probe dispatch or production constructor is used for attestation.
func directDecisionAuditManifest(binding *decisionAuditLocalBinding, mode directAuditMode) decisionAuditManifestV1 {
	return decisionAuditManifestV1{
		binding: binding, version: decisionAuditManifestVersion, source: "fixture/direct-amendment-2",
		profile: fmt.Sprintf("direct-mode-%d", mode), root: "sampled-decision",
		signature:    "EmitDecisionAudit(context.Context,*store.DecisionAuditRecord)",
		ratification: "Amendment2/observable-routing-only", generation: 1, clockEpoch: 1, expires: time.Hour,
		entries: []decisionAuditManifestEntry{
			{ordinal: 0, slot: "root", receiver: "decisionAuditRouter", target: "EmitDecisionAudit", signature: "context,record->void", capture: "adapter+clock+legacy+settings+final-caller", contract: "exclusive-K1", instance: 1, generation: 1},
			{ordinal: 1, slot: "handler", receiver: "directDecisionAuditAdapter", target: "AcceptDecision", signature: "context,record->acceptance,error", capture: "sink+selected-handler-writer+decoy-handler-writer+fixed-mode+local-context+slot", contract: "finite-sync-return-2s", instance: 2, generation: 1},
			{ordinal: 2, slot: "clock", receiver: "auditFixtureClock", target: "Read", signature: "->elapsed,epoch,valid", capture: "tick+epoch+one-final-read-barrier", contract: "injected-elapsed", instance: 3, generation: 1},
			{ordinal: 3, slot: "timer", receiver: "auditFixtureClock", target: "CancelAt", signature: "elapsed,cancel->stop", capture: "one-timer+local-cancel", contract: "finite-single-timer", instance: 3, generation: 1},
			{ordinal: 4, slot: "legacy", receiver: "auditFixtureLegacy", target: "EmitDecisionAudit", signature: "context,record->void", capture: "eight-record-array", contract: "finite-recorder", instance: 4, generation: 1},
			{ordinal: 5, slot: "settings", receiver: "directDecisionAuditSettings", target: "ListHubSettings", signature: "context->bounded-rows,error", capture: "one-row+eight-reads+fixed-failure", contract: "cooperative-read-1s", instance: 5, generation: 1},
			{ordinal: 6, slot: "caller", receiver: "directDecisionAuditCaller", target: "Err/Done/Deadline/Value/Cancel", signature: "bounded-context+cancel->canceled", capture: "parentless-channel+public-meta+route+optional-trace-fixed-chain", contract: "finite-caller-cancellation", instance: 6, generation: 1},
		},
	}
}

func newDirectDecisionAuditFixture(t *testing.T, mode directAuditMode, selectB, traced bool) *directDecisionAuditFixture {
	t.Helper()
	f := &directDecisionAuditFixture{
		clock: &auditFixtureClock{epoch: 1, valid: true}, legacy: &auditFixtureLegacy{},
		base:     &directDecisionAuditCaller{done: make(chan struct{})},
		meta:     &logging.RequestMeta{RequestID: "trusted-direct-correlation"},
		settings: &directDecisionAuditSettings{fakeHubSettingStore: newFakeHubSettingStore()},
	}
	f.installed = logging.ContextWithRequestMeta(f.base, f.meta)
	f.routed = ContextWithRoute(f.installed, "/api/v1/agents/{agentId}")
	f.caller = f.routed
	if traced {
		f.trace = newPerfTrace(nil)
		f.caller = contextWithPerfTrace(f.routed, f.trace)
	}
	srv := &Server{}
	reg, err := experiments.NewRegistry([]experiments.Experiment{{
		Name: experiments.AuthorizationDecisionAuditV2, Title: "Authorization decision audit v2",
		Description: "Finite direct-emission test domain.", Layers: []experiments.Layer{experiments.LayerServer},
		Stage: experiments.StageAlpha, Issue: "ptone/scion#2379", Owner: "audit-update", ReviewBy: "2026-11-30",
	}}, nil)
	require.NoError(t, err)
	srv.experiments = reg
	f.router = newDecisionAuditRouter(f.legacy, srv)
	srv.decisionAuditRouter = f.router
	f.a = &directDecisionAuditHandler{writer: &directDecisionAuditWriter{}, clock: f.clock, caller: f.base, router: f.router}
	f.b = &directDecisionAuditHandler{writer: &directDecisionAuditWriter{}, clock: f.clock, caller: f.base, router: f.router}
	f.writerA, f.writerB = f.a.writer, f.b.writer
	f.loggerA, f.loggerB = slog.New(f.a), slog.New(f.b)
	f.selected, f.decoy = f.a, f.b
	logger := f.loggerA
	if selectB {
		f.selected, f.decoy, logger = f.b, f.a, f.loggerB
	}
	f.selected.mode, f.selected.writer.mode = mode, mode
	f.selected.ready, f.selected.release, f.selected.cancelSeen = make(chan struct{}), make(chan struct{}), make(chan struct{})
	sink, err := auditevent.NewSlogSink(logger)
	require.NoError(t, err)
	f.adapter = &directDecisionAuditAdapter{sink: sink, selected: f.selected, decoy: f.decoy, writer: f.selected.writer, caller: f.caller, router: f.router}
	f.settings.seed("experiments", json.RawMessage(`{"overrides":{"hub.authorization_decision_audit_v2":true}}`))
	f.ops = NewOperationalSettings(f.settings, emptyKoanf(), emptyKoanf())
	srv.SetOperationalSettings(f.ops)
	expected := &decisionAuditLocalBinding{root: f.router, legacy: f.legacy, handler: f.adapter, clock: f.clock, settings: f.ops, caller: f.caller}
	actual := &decisionAuditLocalBinding{root: f.router, legacy: f.legacy, handler: f.adapter, clock: f.clock, settings: f.ops, caller: f.caller}
	f.contract = decisionAuditLocalContract{expected: directDecisionAuditManifest(expected, mode), actual: directDecisionAuditManifest(actual, mode), handler: f.adapter, clock: f.clock}
	require.NoError(t, validateDirectFixtureComposition(f))
	admission, rejection := validateDecisionAuditManifest(f.contract)
	require.NotNil(t, admission, "finite manifest rejected: %s", rejection)
	f.router.contract, f.router.admission = f.contract, admission
	return f
}

// Public and directly owned references only. The sink's private logger identity
// is intentionally not inspected; the actual A/B dispatch supplies that oracle.
func validateDirectFixtureComposition(f *directDecisionAuditFixture) error {
	if f.adapter.sink == nil || f.loggerA.Handler() != f.a || f.loggerB.Handler() != f.b ||
		f.a == f.b || f.a.writer == nil || f.b.writer == nil || f.a.writer == f.b.writer ||
		f.a.writer != f.writerA || f.b.writer != f.writerB || f.adapter.writer != f.selected.writer ||
		f.adapter.selected != f.selected || f.adapter.decoy != f.decoy ||
		f.adapter.caller != f.caller || f.contract.expected.binding.caller != f.caller ||
		f.contract.actual.binding.caller != f.caller ||
		logging.RequestMetaFromContext(f.caller) != f.meta || logging.RequestIDFromContext(f.caller) != f.meta.RequestID ||
		routeFromContext(f.caller) != "/api/v1/agents/{agentId}" || perfTraceFrom(f.caller) != f.trace {
		return errors.New("direct fixture composition mismatch")
	}
	return nil
}

func (f *directDecisionAuditFixture) refresh(t *testing.T, enabled bool) {
	t.Helper()
	f.settings.mu.Lock()
	row := f.settings.settings["experiments"]
	next := json.RawMessage(fmt.Sprintf(`{"overrides":{"hub.authorization_decision_audit_v2":%t}}`, enabled))
	if !bytes.Equal(row.Value, next) {
		row.Revision++
	}
	row.Value = next
	f.settings.mu.Unlock()
	_, err := f.ops.Refresh(context.Background())
	require.NoError(t, err)
}

func validReturnedDirectDecision(allowed bool) (AuthzRequest, Decision) {
	return AuthzRequest{
			Principal: PrincipalContext{Kind: PrincipalKindUser, ID: "conflicting-request-principal"},
			Resource:  Resource{Type: "agent", ID: "agent-direct", Labels: map[string]string{"canary": "Bearer direct-token-canary"}, Ancestry: []string{"parent-direct"}},
			Action:    ActionRead, Permission: "agent.read",
		}, Decision{Allowed: allowed, Reason: "finite returned decision", PrincipalKind: PrincipalKindUser,
			PrincipalID: "evaluated-direct-principal", PermissionID: "agent.read", principalDecorated: true}
}

type directEmissionObservation struct {
	requestBefore, requestAfter, decisionBefore, decisionAfter []byte
	decorated                                                  bool
}

// Workers collect bounded observations only; all testing control flow belongs
// to the controlling goroutine, including failure cleanup.
func (f *directDecisionAuditFixture) observeEmission(request AuthzRequest, decision Decision, rate float64) (o directEmissionObservation, err error) {
	o.requestBefore, err = json.Marshal(request)
	if err != nil {
		return o, err
	}
	o.decisionBefore, err = json.Marshal(decision)
	if err != nil {
		return o, err
	}
	emitter := wrapAuditEmitterForPerfTrace(f.router, f.trace != nil)
	service := &AuthzService{decisionAuditEmitter: emitter, DecisionAuditSampleRate: rate}
	service.emitDecisionAudit(f.caller, request, decision)
	o.requestAfter, err = json.Marshal(request)
	if err != nil {
		return o, err
	}
	o.decisionAfter, err = json.Marshal(decision)
	o.decorated = decision.principalDecorated
	return o, err
}

func directCheckEmission(t *testing.T, o directEmissionObservation, err error) {
	t.Helper()
	assert.NoError(t, err)
	assert.Equal(t, o.requestBefore, o.requestAfter)
	assert.Equal(t, o.decisionBefore, o.decisionAfter)
	assert.True(t, o.decorated)
}

func (f *directDecisionAuditFixture) emit(t *testing.T, request AuthzRequest, decision Decision, rate float64) {
	t.Helper()
	o, err := f.observeEmission(request, decision, rate)
	directCheckEmission(t, o, err)
}

type directWorkerResult struct {
	observation directEmissionObservation
	err         error
	returned    bool
}

type directWorker struct {
	done   chan struct{}
	result chan directWorkerResult
	joined bool
	value  directWorkerResult
}

func newDirectWorker() *directWorker {
	return &directWorker{done: make(chan struct{}), result: make(chan directWorkerResult, 1)}
}

func (w *directWorker) finish(r *directWorkerResult) {
	if recovered := recover(); recovered != nil {
		r.err = errors.New("direct fixture worker panicked")
	}
	w.result <- *r
	close(w.result)
	close(w.done)
}

func (w *directWorker) join() directWorkerResult {
	if !w.joined {
		<-w.done
		w.value = <-w.result
		w.joined = true
	}
	return w.value
}

// Exactly one emission and at most one close worker belong to this finite
// controller. Cleanup is registered before launch, releases both barriers once,
// cancels/drains the router, joins every worker, then checks every result.
type directWorkers struct {
	f                        *directDecisionAuditFixture
	emission, closing        *directWorker
	cleanupRelease           chan struct{}
	handlerOnce, cleanupOnce sync.Once
}

func newDirectWorkers(t *testing.T, f *directDecisionAuditFixture, cleanupRelease chan struct{}) *directWorkers {
	t.Helper()
	w := &directWorkers{f: f, cleanupRelease: cleanupRelease}
	t.Cleanup(func() {
		w.releaseHandler()
		w.releaseCleanup()
		closeErr := f.router.CloseNew(context.Background())
		var emission, closing directWorkerResult
		if w.emission != nil {
			emission = w.emission.join()
		}
		if w.closing != nil {
			closing = w.closing.join()
		}
		assert.NoError(t, closeErr)
		if w.emission != nil {
			assert.True(t, emission.returned, "emission worker must return normally")
			directCheckEmission(t, emission.observation, emission.err)
		}
		if w.closing != nil {
			assert.True(t, closing.returned, "close worker must return normally")
			assert.NoError(t, closing.err)
		}
	})
	return w
}

func (w *directWorkers) releaseHandler() {
	w.handlerOnce.Do(func() { close(w.f.selected.release) })
}

func (w *directWorkers) releaseCleanup() {
	if w.cleanupRelease != nil {
		w.cleanupOnce.Do(func() { close(w.cleanupRelease) })
	}
}

func (w *directWorkers) startEmission(request AuthzRequest, decision Decision, rate float64) {
	w.emission = newDirectWorker()
	worker, f := w.emission, w.f
	go func() {
		var r directWorkerResult
		defer worker.finish(&r)
		r.observation, r.err = f.observeEmission(request, decision, rate)
		r.returned = true
	}()
}

func (w *directWorkers) startClose() {
	w.closing = newDirectWorker()
	worker, router := w.closing, w.f.router
	go func() {
		var r directWorkerResult
		defer worker.finish(&r)
		r.err = router.CloseNew(context.Background())
		r.returned = true
	}()
}

func (w *directWorkers) joinEmission(t *testing.T) {
	t.Helper()
	auditFixtureWait(w.emission.done)
	r := w.emission.join()
	assert.True(t, r.returned)
	directCheckEmission(t, r.observation, r.err)
}

func (w *directWorkers) joinClose(t *testing.T) {
	t.Helper()
	auditFixtureWait(w.closing.done)
	r := w.closing.join()
	assert.True(t, r.returned)
	assert.NoError(t, r.err)
}

func directRequireReleased(t *testing.T, f *directDecisionAuditFixture) {
	t.Helper()
	assert.Zero(t, f.router.inspect().active)
	assert.Nil(t, f.router.slot)
	assert.Nil(t, f.clock.cancel)
	if slot := f.adapter.slot; slot != nil {
		select {
		case <-slot.done:
		default:
			t.Fatal("direct original completion missing")
		}
		assert.Nil(t, slot.caller)
		assert.Nil(t, slot.record)
		assert.Nil(t, slot.admission)
		assert.Nil(t, slot.handler)
		assert.Nil(t, slot.clock)
		assert.Nil(t, slot.cancel)
	}
}

func directRequireSuccess(t *testing.T, f *directDecisionAuditFixture, decision Decision) {
	t.Helper()
	require.Equal(t, 1, f.adapter.calls)
	require.Equal(t, 1, f.adapter.emits)
	require.Equal(t, 1, f.selected.enabled)
	require.Equal(t, 1, f.selected.handled)
	require.Equal(t, 1, f.selected.writer.calls)
	require.Equal(t, 1, f.selected.writer.complete)
	assert.Zero(t, f.decoy.enabled)
	assert.Zero(t, f.decoy.handled)
	assert.Zero(t, f.decoy.writer.calls)
	assert.Empty(t, f.legacy.records)
	assert.Zero(t, f.router.inspect().failures)
	assert.False(t, f.router.inspect().fault)
	assert.Equal(t, f.adapter.ctx, f.selected.ctx)
	assert.NotEqual(t, f.caller, f.adapter.ctx)
	assert.Equal(t, f.meta.RequestID, f.adapter.record.CorrelationID)
	assert.Equal(t, decision.PrincipalID, f.adapter.record.PrincipalID)
	assert.Equal(t, decision.PermissionID, f.adapter.record.PermissionID)
	want, err := auditevent.Render(f.adapter.event)
	require.NoError(t, err)
	assert.JSONEq(t, string(want), string(f.selected.writer.data))
	var emitted map[string]any
	require.NoError(t, json.Unmarshal(f.selected.writer.data, &emitted))
	assert.Equal(t, float64(1), emitted["schema_version"])
	assert.Equal(t, "22222222-2222-4222-8222-000000000001", emitted["event_id"])
	assert.Equal(t, "authorization", emitted["family"])
	assert.Equal(t, "decide", emitted["action"])
	assert.Equal(t, "decision", emitted["phase"])
	wantOutcome, wantSeverity := "allow", "info"
	if !decision.Allowed {
		wantOutcome, wantSeverity = "deny", "warning"
	}
	assert.Equal(t, wantOutcome, emitted["outcome"])
	assert.Equal(t, wantSeverity, emitted["severity"])
	assert.Equal(t, f.meta.RequestID, emitted["correlation_id"])
	assert.Equal(t, f.adapter.record.Timestamp.UTC().Format(time.RFC3339Nano), emitted["occurred_at"])
	assert.NotContains(t, emitted, "causation_id")
	assert.NotContains(t, emitted, "initiator")
	assert.NotContains(t, emitted, "executor")
	assert.NotContains(t, emitted, "credential")
	assert.Equal(t, auditevent.EventName, f.selected.record.Message)
	assert.Equal(t, f.adapter.record.Timestamp.UTC(), f.selected.record.Time)
	wantLevel := slog.LevelInfo
	if !decision.Allowed {
		wantLevel = slog.LevelWarn
	}
	assert.Equal(t, wantLevel, f.selected.record.Level)
	frame, _ := runtime.CallersFrames([]uintptr{f.selected.record.PC}).Next()
	assert.Contains(t, frame.Function, "(*directDecisionAuditAdapter).AcceptDecision")
	assert.LessOrEqual(t, len(f.selected.writer.data), decisionAuditRecordMaxBytes)
	directRequireReleased(t, f)
}

func TestDecisionAuditDirectContract_MappingAllowDeny(t *testing.T) {
	for _, kind := range []string{"project", "agent"} {
		for _, cause := range []string{"allow-empty", "deny-unattributed", "deny-delegation-ceiling"} {
			t.Run(kind+"/"+cause, func(t *testing.T) {
				f := newDirectDecisionAuditFixture(t, directAuditAccept, false, false)
				f.refresh(t, true)
				request, decision := validReturnedDirectDecision(cause == "allow-empty")
				request.Resource.Type, request.Resource.ID = kind, kind+"-direct"
				request.Permission, decision.PermissionID = kind+".read", kind+".read"
				if cause == "deny-delegation-ceiling" {
					decision.DeniedBy = DeniedByDelegationCeiling
				}
				f.emit(t, request, decision, 1)
				directRequireSuccess(t, f, decision)
				var got map[string]any
				require.NoError(t, json.Unmarshal(f.selected.writer.data, &got))
				payload := got["payload"].(map[string]any)
				assert.Equal(t, map[string]any{"permission_id": decision.PermissionID, "permission": "read", "reason": decision.Reason, "sampled": "false"}, func() map[string]any {
					copy := make(map[string]any)
					for key, value := range payload {
						if key != "denied_by" {
							copy[key] = value
						}
					}
					return copy
				}())
				if decision.DeniedBy == "" {
					assert.NotContains(t, payload, "denied_by")
				} else {
					assert.Equal(t, string(decision.DeniedBy), payload["denied_by"])
				}
				assert.Equal(t, map[string]any{"kind": kind, "id": kind + "-direct"}, got["resource"])
				assert.Equal(t, map[string]any{"kind": "user", "id": decision.PrincipalID}, got["principal"])
				assert.Equal(t, map[string]any{"id": f.meta.RequestID, "route": "/api/v1/agents/{agentId}"}, got["request"])
				assert.NotContains(t, got, "credential")
				assert.NotContains(t, got, "operation_id")
			})
		}
	}
	t.Run("returned-permission-conflicts-with-request-and-derived", func(t *testing.T) {
		f := newDirectDecisionAuditFixture(t, directAuditAccept, false, false)
		f.refresh(t, true)
		request, decision := validReturnedDirectDecision(true)
		decision.PermissionID = "agent.update"
		request.Permission = "project.read"
		derived, err := resolveResourcePermission(request.Resource.Type, request.Action)
		require.NoError(t, err)
		require.Equal(t, "agent.read", derived)
		for _, permission := range []string{decision.PermissionID, request.Permission, derived} {
			require.True(t, isKnownPermission(permission))
		}
		require.NotEqual(t, decision.PermissionID, request.Permission)
		require.NotEqual(t, decision.PermissionID, derived)
		require.NotEqual(t, request.Permission, derived)
		// Finite returned-Decision boundary fixture only: no Store or Decide
		// evaluation provenance is asserted by these deliberately conflicting inputs.
		f.emit(t, request, decision, 1)
		directRequireSuccess(t, f, decision)
		assert.Equal(t, decision.PermissionID, f.adapter.record.PermissionID)
		payload, ok := f.adapter.event.Payload.(auditevent.AuthorizationDecisionPayload)
		require.True(t, ok)
		assert.Equal(t, decision.PermissionID, payload.PermissionID)
		assert.NotEqual(t, request.Permission, payload.PermissionID)
		assert.NotEqual(t, derived, payload.PermissionID)
	})
	t.Run("decorated-empty-returned-permission-no-request-fallback", func(t *testing.T) {
		f := newDirectDecisionAuditFixture(t, directAuditAccept, false, false)
		f.refresh(t, true)
		request, decision := validReturnedDirectDecision(false)
		require.True(t, isKnownPermission(request.Permission))
		decision.PermissionID = ""
		require.True(t, decision.principalDecorated)
		f.emit(t, request, decision, 1)
		assert.Equal(t, 1, f.adapter.calls)
		assert.Empty(t, f.adapter.record.PermissionID)
		_, err := buildDirectDecisionEnvelope(&f.adapter.record, "direct-empty-permission")
		assert.Error(t, err, "mapper rejects absent returned permission")
		assert.Zero(t, f.adapter.emits)
		assert.Zero(t, f.a.enabled)
		assert.Zero(t, f.b.enabled)
		assert.Zero(t, f.a.handled)
		assert.Zero(t, f.b.handled)
		assert.Zero(t, f.writerA.calls)
		assert.Zero(t, f.writerB.calls)
		assert.EqualValues(t, 1, f.router.inspect().failures)
		assert.Empty(t, f.legacy.records)
		directRequireReleased(t, f)
	})
	for _, tc := range []struct {
		name   string
		mutate func(*store.DecisionAuditRecord)
	}{
		{"decorated-empty-principal-no-fallback", func(r *store.DecisionAuditRecord) { r.PrincipalID = "" }},
		{"permission-absent", func(r *store.DecisionAuditRecord) { r.PermissionID = "" }},
		{"permission-empty", func(r *store.DecisionAuditRecord) { r.PermissionID = "" }},
		{"permission-unknown", func(r *store.DecisionAuditRecord) { r.PermissionID = "unknown.canary" }},
		{"permission-invalid-utf8", func(r *store.DecisionAuditRecord) { r.PermissionID = string([]byte{0xff}) }},
		{"permission-control", func(r *store.DecisionAuditRecord) { r.PermissionID = "agent.read\n" }},
		{"permission-over-cap", func(r *store.DecisionAuditRecord) { r.PermissionID = strings.Repeat("x", 129) }},
		{"correlation-metadata-absent", func(r *store.DecisionAuditRecord) { r.CorrelationID = "" }},
		{"correlation-empty", func(r *store.DecisionAuditRecord) { r.CorrelationID = "" }},
		{"correlation-invalid-utf8", func(r *store.DecisionAuditRecord) { r.CorrelationID = string([]byte{0xff}) }},
		{"correlation-control", func(r *store.DecisionAuditRecord) { r.CorrelationID = "trusted\ncanary" }},
		{"correlation-over-cap", func(r *store.DecisionAuditRecord) { r.CorrelationID = strings.Repeat("x", 129) }},
		{"unsupported-principal", func(r *store.DecisionAuditRecord) { r.PrincipalKind = "broker" }},
		{"unsupported-resource", func(r *store.DecisionAuditRecord) { r.ResourceType = "skill" }},
		{"unsupported-credential", func(r *store.DecisionAuditRecord) { r.CredentialID = "credential-canary" }},
		{"unsupported-credential-type", func(r *store.DecisionAuditRecord) { r.CredentialType = "type-canary" }},
		{"unsupported-name", func(r *store.DecisionAuditRecord) { r.CredentialName = "name-canary" }},
		{"unsupported-boundary", func(r *store.DecisionAuditRecord) { r.CredentialBoundaryKind = "hub" }},
		{"unsupported-project-boundary", func(r *store.DecisionAuditRecord) { r.CredentialBoundaryProjectID = "project-canary" }},
		{"unsupported-labels", func(r *store.DecisionAuditRecord) { r.CredentialLabels = "{}" }},
		{"unsupported-executor-kind", func(r *store.DecisionAuditRecord) { r.ExecutorKind = "agent" }},
		{"unsupported-executor-id", func(r *store.DecisionAuditRecord) { r.ExecutorID = "executor-canary" }},
		{"payload-permission-over-cap", func(r *store.DecisionAuditRecord) { r.Permission = strings.Repeat("x", 129) }},
		{"payload-reason-over-cap", func(r *store.DecisionAuditRecord) { r.Reason = strings.Repeat("x", 257) }},
		{"payload-denied-by-over-cap", func(r *store.DecisionAuditRecord) { r.DeniedBy = strings.Repeat("x", 65) }},
		{"unsupported-result", func(r *store.DecisionAuditRecord) { r.Result = "unknown" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDirectDecisionAuditFixture(t, directAuditAccept, false, false)
			f.refresh(t, true)
			request, decision := validReturnedDirectDecision(false)
			record := BuildDecisionAuditRecord(f.caller, request, decision)
			tc.mutate(record)
			before := *record
			event, err := buildDirectDecisionEnvelope(record, "22222222-2222-4222-8222-000000000001")
			require.Error(t, err)
			assert.Equal(t, auditevent.EnvelopeV1{}, event)
			assert.Equal(t, before, *record)
			assert.NotContains(t, err.Error(), "canary")
			f.router.EmitDecisionAudit(f.caller, record)
			assert.Equal(t, 1, f.adapter.calls)
			assert.Zero(t, f.adapter.emits)
			assert.Zero(t, f.selected.enabled)
			assert.Zero(t, f.selected.handled)
			assert.Zero(t, f.selected.writer.calls)
			assert.Empty(t, f.legacy.records)
			assert.EqualValues(t, 1, f.router.inspect().failures)
			assert.True(t, f.router.inspect().fault)
			f.emit(t, request, decision, 1)
			assert.Len(t, f.legacy.records, 1)
			assert.Equal(t, 1, f.adapter.calls)
			directRequireReleased(t, f)
		})
	}
	t.Run("public-correlation-source-negatives", func(t *testing.T) {
		for _, value := range []string{"", string([]byte{0xff}), "trusted\ncanary", strings.Repeat("x", 129)} {
			base := &directDecisionAuditCaller{done: make(chan struct{})}
			request, decision := validReturnedDirectDecision(true)
			for _, ctx := range []context.Context{base, logging.ContextWithRequestMeta(base, &logging.RequestMeta{RequestID: value})} {
				record := BuildDecisionAuditRecord(ctx, request, decision)
				_, err := buildDirectDecisionEnvelope(record, "22222222-2222-4222-8222-000000000001")
				require.Error(t, err)
			}
		}
	})
	t.Run("returned-empty-principal-preserved", func(t *testing.T) {
		f := newDirectDecisionAuditFixture(t, directAuditAccept, false, false)
		request, decision := validReturnedDirectDecision(false)
		decision.PrincipalID = ""
		record := BuildDecisionAuditRecord(f.caller, request, decision)
		assert.Empty(t, record.PrincipalID)
		_, err := buildDirectDecisionEnvelope(record, "22222222-2222-4222-8222-000000000001")
		require.Error(t, err)
	})
	t.Run("payload-at-cap-UTC-preserves-instant", func(t *testing.T) {
		f := newDirectDecisionAuditFixture(t, directAuditAccept, false, false)
		request, decision := validReturnedDirectDecision(false)
		record := BuildDecisionAuditRecord(f.caller, request, decision)
		record.Permission, record.Reason, record.DeniedBy = strings.Repeat("p", 128), strings.Repeat("r", 256), strings.Repeat("d", 64)
		record.CorrelationID = strings.Repeat("c", 128)
		record.Timestamp = time.Date(2026, 10, 8, 12, 34, 56, 123, time.FixedZone("finite-offset", 3600))
		before := *record
		event, err := buildDirectDecisionEnvelope(record, "22222222-2222-4222-8222-000000000001")
		require.NoError(t, err)
		assert.True(t, record.Timestamp.Equal(event.OccurredAt))
		assert.Equal(t, time.UTC, event.OccurredAt.Location())
		assert.Equal(t, before, *record)
	})
}

func TestDecisionAuditDirectContract_ContextChain(t *testing.T) {
	f := newDirectDecisionAuditFixture(t, directAuditAccept, false, true)
	for _, ctx := range []context.Context{f.installed, f.routed, f.caller} {
		assert.Same(t, f.meta, logging.RequestMetaFromContext(ctx))
		assert.Equal(t, "trusted-direct-correlation", logging.RequestIDFromContext(ctx))
	}
	assert.Equal(t, "/api/v1/agents/{agentId}", routeFromContext(f.caller))
	assert.Same(t, f.trace, perfTraceFrom(f.caller))
	assert.Equal(t, f.caller, f.contract.expected.binding.caller)
	assert.Equal(t, f.caller, f.contract.actual.binding.caller)
	require.NoError(t, validateDirectFixtureComposition(f))
	f.refresh(t, true)
	request, decision := validReturnedDirectDecision(true)
	for _, ctx := range []context.Context{f.base, f.installed, f.routed, ContextWithRoute(f.installed, "/api/v1/agents/{agentId}")} {
		f.router.EmitDecisionAudit(ctx, BuildDecisionAuditRecord(ctx, request, decision))
	}
	assert.Len(t, f.legacy.records, 4)
	assert.Zero(t, f.adapter.calls)
	f.legacy.records = nil
	f.emit(t, request, decision, 1)
	directRequireSuccess(t, f, decision)
	assert.Nil(t, logging.RequestMetaFromContext(f.adapter.ctx))
	assert.Empty(t, logging.RequestIDFromContext(f.adapter.ctx))
	assert.Nil(t, perfTraceFrom(f.adapter.ctx))
	assert.Equal(t, f.meta.RequestID, f.adapter.event.CorrelationID)
	assert.Equal(t, f.meta.RequestID, f.adapter.event.Request.ID)
}

func TestDecisionAuditDirectContract_NestedBinding(t *testing.T) {
	for _, selectB := range []bool{false, true} {
		t.Run(fmt.Sprintf("selected-B-%t", selectB), func(t *testing.T) {
			f := newDirectDecisionAuditFixture(t, directAuditAccept, selectB, false)
			assert.Zero(t, f.a.enabled)
			assert.Zero(t, f.b.enabled)
			assert.Zero(t, f.adapter.emits, "composition must not secretly dispatch a probe")
			f.refresh(t, true)
			request, decision := validReturnedDirectDecision(true)
			f.emit(t, request, decision, 1)
			directRequireSuccess(t, f, decision)
		})
	}
	t.Run("swapped-actual-sink-observable", func(t *testing.T) {
		f := newDirectDecisionAuditFixture(t, directAuditAccept, false, false)
		f.refresh(t, true)
		sink, err := auditevent.NewSlogSink(f.loggerB)
		require.NoError(t, err)
		f.adapter.sink = sink
		request, decision := validReturnedDirectDecision(true)
		f.emit(t, request, decision, 1)
		assert.Zero(t, f.a.enabled)
		assert.Zero(t, f.a.handled)
		assert.Zero(t, f.a.writer.calls)
		assert.Equal(t, 1, f.b.enabled)
		assert.Equal(t, 1, f.b.handled)
		assert.Equal(t, 1, f.b.writer.complete)
		assert.True(t, f.router.inspect().fault, "decoy dispatch is not selected-chain acceptance")
		assert.Empty(t, f.legacy.records)
		directRequireReleased(t, f)
	})
	t.Run("two-loggers-same-handler-observationally-equivalent", func(t *testing.T) {
		f := newDirectDecisionAuditFixture(t, directAuditAccept, false, false)
		f.refresh(t, true)
		equivalent := slog.New(f.a)
		assert.NotSame(t, f.loggerA, equivalent)
		sink, err := auditevent.NewSlogSink(equivalent)
		require.NoError(t, err)
		f.adapter.sink = sink
		request, decision := validReturnedDirectDecision(true)
		f.emit(t, request, decision, 1)
		directRequireSuccess(t, f, decision)
	})
	t.Run("disabled-nil-is-not-proof", func(t *testing.T) {
		f := newDirectDecisionAuditFixture(t, directAuditDisabled, false, false)
		f.refresh(t, true)
		request, decision := validReturnedDirectDecision(true)
		f.emit(t, request, decision, 1)
		assert.Equal(t, 1, f.selected.enabled)
		assert.Zero(t, f.selected.handled)
		assert.Zero(t, f.selected.writer.calls)
		assert.Zero(t, f.decoy.enabled)
		assert.True(t, f.router.inspect().fault)
		assert.EqualValues(t, 1, f.router.inspect().failures)
		directRequireReleased(t, f)
	})
	t.Run("swapped-writer-rejected-before-sink-dispatch", func(t *testing.T) {
		f := newDirectDecisionAuditFixture(t, directAuditAccept, false, false)
		f.refresh(t, true)
		f.a.writer = &directDecisionAuditWriter{}
		require.Error(t, validateDirectFixtureComposition(f))
		request, decision := validReturnedDirectDecision(true)
		f.emit(t, request, decision, 1)
		assert.Equal(t, 1, f.adapter.calls)
		assert.Zero(t, f.adapter.emits)
		assert.Zero(t, f.a.enabled)
		assert.Zero(t, f.writerA.calls)
		assert.Zero(t, f.a.writer.calls)
		assert.True(t, f.router.inspect().fault)
		assert.Empty(t, f.legacy.records)
		directRequireReleased(t, f)
	})
	t.Run("swapped-selected-handler-constructor-rejected", func(t *testing.T) {
		f := newDirectDecisionAuditFixture(t, directAuditAccept, false, false)
		f.adapter.selected = f.b
		require.Error(t, validateDirectFixtureComposition(f))
		assert.Zero(t, f.adapter.calls)
		assert.Zero(t, f.a.enabled)
		assert.Zero(t, f.b.enabled)
	})
}

func TestDecisionAuditDirectContract_SynchronousOnce(t *testing.T) {
	for _, allowed := range []bool{true, false} {
		for _, traced := range []bool{false, true} {
			t.Run(fmt.Sprintf("allow-%t-trace-%t", allowed, traced), func(t *testing.T) {
				f := newDirectDecisionAuditFixture(t, directAuditAccept, false, traced)
				f.refresh(t, true)
				request, decision := validReturnedDirectDecision(allowed)
				f.emit(t, request, decision, 1)
				directRequireSuccess(t, f, decision)
				assert.Equal(t, 1, f.clock.timerStarts)
				assert.Equal(t, 1, f.settings.calls)
			})
		}
	}
}

func TestDecisionAuditDirectContract_DisabledAdmission(t *testing.T) {
	for _, name := range []string{
		"default-off", "explicit-false", "missing", "malformed", "stale", "unknown-experiment",
		"missing-contract", "mismatched-contract", "expired-contract",
		"swapped-handler", "swapped-writer", "swapped-caller", "clock-epoch", "clock-regression",
	} {
		t.Run(name, func(t *testing.T) {
			f := newDirectDecisionAuditFixture(t, directAuditAccept, false, false)
			if name != "default-off" {
				f.refresh(t, true)
			}
			switch name {
			case "explicit-false":
				f.refresh(t, false)
			case "missing":
				f.settings.mu.Lock()
				delete(f.settings.settings, "experiments")
				f.settings.mu.Unlock()
				_, err := f.ops.Refresh(context.Background())
				require.NoError(t, err)
			case "malformed":
				f.settings.mu.Lock()
				f.settings.settings["experiments"].Value = json.RawMessage(`{"overrides":`)
				f.settings.settings["experiments"].Revision++
				f.settings.mu.Unlock()
				_, _ = f.ops.Refresh(context.Background())
			case "stale":
				f.clock.advance(decisionAuditLease)
			case "unknown-experiment":
				reg, err := experiments.NewRegistry(nil, nil)
				require.NoError(t, err)
				f.router.server.experiments = reg
			case "missing-contract":
				f.router.admission = nil
			case "mismatched-contract":
				f.contract.actual.entries[1].capture = "drift"
				admission, _ := validateDecisionAuditManifest(f.contract)
				assert.Nil(t, admission)
				f.router.admission = admission
			case "expired-contract":
				f.clock.tick = f.contract.expected.expires
				admission, _ := validateDecisionAuditManifest(f.contract)
				assert.Nil(t, admission)
				f.router.admission = admission
			case "swapped-handler":
				f.contract.actual.binding.handler = &directDecisionAuditAdapter{}
				admission, _ := validateDecisionAuditManifest(f.contract)
				assert.Nil(t, admission)
				f.router.admission = admission
			case "swapped-writer":
				f.a.writer = &directDecisionAuditWriter{}
				require.Error(t, validateDirectFixtureComposition(f))
				f.router.admission = nil // rejected constructor, never admitted as a new graph
			case "swapped-caller":
				f.caller = ContextWithRoute(f.installed, "/api/v1/agents/{agentId}")
				require.Error(t, validateDirectFixtureComposition(f))
			case "clock-epoch":
				f.clock.epoch++
			case "clock-regression":
				f.clock.tick = time.Second
				f.refresh(t, true)
				f.clock.tick = 0
			}
			timers := f.clock.timerStarts
			request, decision := validReturnedDirectDecision(true)
			f.emit(t, request, decision, 1)
			assert.Len(t, f.legacy.records, 1)
			assert.Zero(t, f.adapter.calls)
			assert.Zero(t, f.adapter.emits)
			assert.Zero(t, f.a.enabled)
			assert.Zero(t, f.b.enabled)
			assert.Equal(t, timers, f.clock.timerStarts)
			assert.Zero(t, f.router.inspect().failures)
			directRequireReleased(t, f)
		})
	}
	t.Run("capacity-owned", func(t *testing.T) {
		f := newDirectDecisionAuditFixture(t, directAuditControlled, false, false)
		f.refresh(t, true)
		request, decision := validReturnedDirectDecision(true)
		workers := newDirectWorkers(t, f, nil)
		workers.startEmission(request, decision, 1)
		auditFixtureWait(f.selected.ready)
		f.emit(t, request, decision, 1)
		assert.Len(t, f.legacy.records, 1)
		assert.Equal(t, 1, f.adapter.calls)
		assert.Equal(t, decisionAuditNewCapacity, f.router.inspect().active)
		workers.releaseHandler()
		workers.joinEmission(t)
		assert.Equal(t, 1, f.selected.writer.complete)
		assert.Zero(t, f.router.inspect().failures)
		directRequireReleased(t, f)
	})
}

func TestDecisionAuditDirectContract_SinkFailureHealthAndNextLegacy(t *testing.T) {
	for _, mode := range []directAuditMode{directAuditDisabled, directAuditHandlerError, directAuditHandlerPanic,
		directAuditWriterError, directAuditWriterPanic, directAuditShortWrite, directAuditZeroWrite,
		directAuditLocalCancel, directAuditCallerCancel, directAuditTimer, directAuditCompleteLate} {
		t.Run(fmt.Sprintf("mode-%d", mode), func(t *testing.T) {
			f := newDirectDecisionAuditFixture(t, mode, false, true)
			f.refresh(t, true)
			request, decision := validReturnedDirectDecision(false)
			f.emit(t, request, decision, 1)
			assert.Equal(t, 1, f.adapter.calls)
			assert.Equal(t, 1, f.adapter.emits)
			assert.Equal(t, 1, f.selected.enabled)
			wantHandled, wantWritten, wantComplete := 1, 0, 0
			if mode == directAuditDisabled {
				wantHandled = 0
			}
			switch mode {
			case directAuditWriterError, directAuditWriterPanic, directAuditShortWrite, directAuditZeroWrite:
				wantWritten = 1
			case directAuditCallerCancel, directAuditCompleteLate:
				wantWritten, wantComplete = 1, 1
			}
			assert.Equal(t, wantHandled, f.selected.handled)
			assert.Equal(t, wantWritten, f.selected.writer.calls)
			assert.Equal(t, wantComplete, f.selected.writer.complete)
			assert.Zero(t, f.decoy.enabled)
			assert.Empty(t, f.legacy.records, "failed owned record must not fall back")
			assert.EqualValues(t, 1, f.router.inspect().failures)
			assert.True(t, f.router.inspect().fault)
			// f.legacy is the test-local auditFixtureLegacy recorder injected into the
			// router's legacy slot; it observes routing and persists nothing. Production
			// wires inertDecisionAuditTarget there, so the warning reports no persistence
			// and the retired writer has no health value.
			assert.Equal(t, "unhealthy: CRITICAL authorization decision logging fault; NEW off; triggering record may be lost; subsequent decisions have no persistence", f.router.healthProjection())
			assert.False(t, f.router.inspect().observation.successful)
			directRequireReleased(t, f)
			// Healthy true settings cannot rearm a faulted generation.
			f.refresh(t, true)
			f.emit(t, request, decision, 1)
			assert.Len(t, f.legacy.records, 1)
			assert.Equal(t, "deny", f.legacy.records[0].Result)
			assert.Equal(t, decision.PermissionID, f.legacy.records[0].PermissionID)
			assert.Equal(t, f.meta.RequestID, f.legacy.records[0].CorrelationID)
			assert.Equal(t, 1, f.adapter.calls)
			assert.EqualValues(t, 1, f.router.inspect().failures)
			directRequireReleased(t, f)
		})
	}
}

func TestDecisionAuditDirectContract_Cancellation(t *testing.T) {
	t.Run("before-handoff", func(t *testing.T) {
		f := newDirectDecisionAuditFixture(t, directAuditAccept, false, false)
		f.refresh(t, true)
		f.base.Cancel()
		request, decision := validReturnedDirectDecision(false)
		f.emit(t, request, decision, 1)
		assert.Len(t, f.legacy.records, 1)
		assert.Zero(t, f.adapter.calls)
		assert.Zero(t, f.router.inspect().failures)
		directRequireReleased(t, f)
	})
	for _, mode := range []directAuditMode{directAuditCallerCancel, directAuditLocalCancel, directAuditTimer, directAuditCompleteAt, directAuditCompleteLate} {
		t.Run(fmt.Sprintf("owned-mode-%d", mode), func(t *testing.T) {
			f := newDirectDecisionAuditFixture(t, mode, false, false)
			f.refresh(t, true)
			request, decision := validReturnedDirectDecision(true)
			f.emit(t, request, decision, 1)
			assert.Empty(t, f.legacy.records)
			assert.Equal(t, 1, f.adapter.calls)
			if mode == directAuditCompleteAt {
				directRequireSuccess(t, f, decision)
			} else {
				assert.True(t, f.router.inspect().fault)
				assert.EqualValues(t, 1, f.router.inspect().failures)
			}
			if mode == directAuditTimer {
				assert.True(t, f.selected.boundaryOK)
			}
			if mode == directAuditCallerCancel {
				assert.ErrorIs(t, f.caller.Err(), context.Canceled)
			}
			directRequireReleased(t, f)
		})
	}
}

func TestDecisionAuditDirectContract_SamplingAndPrivacy(t *testing.T) {
	for _, tc := range []struct {
		name                                         string
		rate                                         float64
		allowed, requestAlways, decisionAlways, emit bool
	}{
		{"rate1-allow", 1, true, false, false, true}, {"rate1-deny", 1, false, false, false, true},
		{"rate0-allow-skipped", 0, true, false, false, false}, {"rate0-deny", 0, false, false, false, true},
		{"request-always", 0, true, true, false, true}, {"decision-always", 0, true, false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDirectDecisionAuditFixture(t, directAuditAccept, false, true)
			f.refresh(t, true)
			request, decision := validReturnedDirectDecision(tc.allowed)
			request.AlwaysAudit, decision.AlwaysAudit = tc.requestAlways, tc.decisionAlways
			request.Purpose = "Bearer direct-token-canary"
			request.Credential.Scopes = []string{"Bearer direct-token-canary"}
			f.emit(t, request, decision, tc.rate)
			if !tc.emit {
				assert.Zero(t, f.adapter.calls)
				assert.Zero(t, f.trace.Snapshot().AuditRecords)
				assert.Empty(t, f.legacy.records)
				return
			}
			directRequireSuccess(t, f, decision)
			assert.Equal(t, tc.rate < 1, f.adapter.record.Sampled)
			var got map[string]any
			require.NoError(t, json.Unmarshal(f.selected.writer.data, &got))
			assert.Equal(t, strconv.FormatBool(tc.rate < 1), got["payload"].(map[string]any)["sampled"])
			assert.False(t, bytes.Contains(f.selected.writer.data, []byte("direct-token-canary")))
			assert.False(t, bytes.Contains(f.selected.writer.data, []byte("conflicting-request-principal")))
			assert.NotContains(t, string(f.selected.writer.data), "Bearer")
			assert.EqualValues(t, 1, f.trace.Snapshot().AuditRecords)
		})
	}
}

func TestDecisionAuditDirectContract_FreshnessAndDrain(t *testing.T) {
	t.Run("unchanged-renewal", func(t *testing.T) {
		f := newDirectDecisionAuditFixture(t, directAuditAccept, false, false)
		f.refresh(t, true)
		first := f.router.inspect().observation
		f.clock.advance(time.Second)
		f.refresh(t, true)
		renewed := f.router.inspect().observation
		assert.True(t, renewed.successful)
		assert.Greater(t, renewed.sequence, first.sequence)
		assert.Equal(t, f.clock.tick+decisionAuditLease, renewed.deadline)
		request, decision := validReturnedDirectDecision(true)
		f.emit(t, request, decision, 1)
		directRequireSuccess(t, f, decision)
	})
	for _, name := range []string{"false-refresh", "failed-refresh", "malformed-refresh", "stale-lease", "detach", "disconnect"} {
		t.Run(name, func(t *testing.T) {
			f := newDirectDecisionAuditFixture(t, directAuditAccept, false, false)
			f.refresh(t, true)
			switch name {
			case "false-refresh":
				f.refresh(t, false)
			case "failed-refresh":
				f.settings.fail = true
				_, err := f.ops.Refresh(context.Background())
				require.Error(t, err)
			case "malformed-refresh":
				f.settings.mu.Lock()
				f.settings.settings["experiments"].Value = json.RawMessage(`{"overrides":`)
				f.settings.settings["experiments"].Revision++
				f.settings.mu.Unlock()
				_, _ = f.ops.Refresh(context.Background())
			case "stale-lease":
				f.clock.advance(decisionAuditLease)
			case "detach":
				f.router.server.SetOperationalSettings(nil)
			case "disconnect":
				f.ops.StopPropagation()
			}
			request, decision := validReturnedDirectDecision(false)
			f.emit(t, request, decision, 1)
			assert.Len(t, f.legacy.records, 1)
			assert.Zero(t, f.adapter.calls)
			assert.Zero(t, f.router.inspect().failures)
			directRequireReleased(t, f)
		})
	}
	t.Run("active-close-gate-free-original-drain", func(t *testing.T) {
		f := newDirectDecisionAuditFixture(t, directAuditActiveClose, false, true)
		f.refresh(t, true)
		request, decision := validReturnedDirectDecision(false)
		workers := newDirectWorkers(t, f, nil)
		workers.startEmission(request, decision, 1)
		auditFixtureWait(f.selected.ready)
		slot := f.adapter.slot
		workers.startClose()
		auditFixtureWait(f.selected.cancelSeen)
		// This later probe observes gate availability when the handler resumes;
		// it does not establish the gate state at the cancellation site.
		assert.True(t, f.selected.gateFree, "gate available when handler resumes after cancellation")
		assert.Equal(t, decisionAuditNewCapacity, f.router.inspect().active)
		select {
		case <-workers.closing.done:
			t.Fatal("close abandoned the original slot")
		default:
		}
		assert.Zero(t, f.trace.Snapshot().AuditRecords)
		workers.releaseHandler()
		workers.joinEmission(t)
		workers.joinClose(t)
		assert.Same(t, slot, f.adapter.slot)
		directRequireReleased(t, f)
		assert.True(t, f.router.inspect().closed)
		assert.EqualValues(t, 1, f.router.inspect().failures)
		assert.Empty(t, f.legacy.records)
		f.emit(t, request, decision, 1)
		assert.Len(t, f.legacy.records, 1, "closing NEW preserves the separately owned legacy path")
		assert.Equal(t, 1, f.adapter.calls)
	})
}

func TestDecisionAuditDirectContract_DispatchTiming(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		allowed, enabled, traced bool
		rate                     float64
		mode                     directAuditMode
		count                    int64
	}{
		{"allow", true, true, true, 1, directAuditAccept, 1},
		{"deny", false, true, true, 1, directAuditAccept, 1},
		{"NEW-failure-cleanup", false, true, true, 1, directAuditHandlerError, 1},
		{"legacy-route", true, false, true, 1, directAuditAccept, 1},
		{"sampled-skip", true, true, true, 0, directAuditAccept, 0},
		{"trace-absent", true, true, false, 1, directAuditAccept, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDirectDecisionAuditFixture(t, tc.mode, false, tc.traced)
			f.refresh(t, tc.enabled)
			request, decision := validReturnedDirectDecision(tc.allowed)
			f.emit(t, request, decision, tc.rate)
			directRequireReleased(t, f)
			snap := f.trace.Snapshot()
			assert.Equal(t, tc.count, snap.AuditRecords)
			if tc.count == 0 {
				assert.Zero(t, snap.AuditEmitTime)
				return
			}
			if tc.allowed {
				assert.Equal(t, tc.count, snap.AuditAllow)
			} else {
				assert.Equal(t, tc.count, snap.AuditDeny)
			}
			assert.Zero(t, snap.AuditOther)
			assert.GreaterOrEqual(t, snap.AuditEmitTime, time.Duration(0))
			found := false
			for _, attr := range snap.LogAttrs() {
				if attr.Key == "audit_emit_us" {
					found = true
					assert.Equal(t, microseconds(snap.AuditEmitTime), attr.Value.Int64())
				}
			}
			assert.True(t, found)
			if !tc.enabled {
				assert.Len(t, f.legacy.records, 1)
				assert.Zero(t, f.adapter.calls)
			}
		})
	}
	t.Run("account-only-after-handler-and-cleanup-return", func(t *testing.T) {
		f := newDirectDecisionAuditFixture(t, directAuditControlled, false, true)
		f.refresh(t, true)
		request, decision := validReturnedDirectDecision(true)
		entered, release := make(chan struct{}), make(chan struct{})
		workers := newDirectWorkers(t, f, release)
		workers.startEmission(request, decision, 1)
		auditFixtureWait(f.selected.ready)
		bracketStart := time.Now()
		assert.Zero(t, f.trace.Snapshot().AuditRecords)
		assert.Equal(t, decisionAuditNewCapacity, f.router.inspect().active)
		f.clock.finalEntered, f.clock.finalRelease = entered, release
		workers.releaseHandler()
		auditFixtureWait(entered)
		assert.Equal(t, 1, f.selected.writer.complete)
		assert.Zero(t, f.trace.Snapshot().AuditRecords, "synchronous cleanup still inside dispatch interval")
		assert.Equal(t, decisionAuditNewCapacity, f.router.inspect().active)
		bracketEnd := time.Now()
		workers.releaseCleanup()
		workers.joinEmission(t)
		directRequireSuccess(t, f, decision)
		snap := f.trace.Snapshot()
		assert.EqualValues(t, 1, snap.AuditRecords)
		assert.Greater(t, bracketEnd.Sub(bracketStart), time.Duration(0))
		assert.GreaterOrEqual(t, snap.AuditEmitTime, bracketEnd.Sub(bracketStart), "dispatch includes independently bracketed handler and cleanup interval")
		found := false
		for _, attr := range snap.LogAttrs() {
			if attr.Key == "audit_emit_us" {
				found = true
				assert.Equal(t, snap.AuditEmitTime.Microseconds(), attr.Value.Int64())
			}
		}
		assert.True(t, found)
	})
}

// This finite fixture graph is test-only. Modes are a closed enum; no arbitrary
// handler callback, production name whitelist or Approved bit grants admission.
// One fixture owns one clock, one timer slot, one handler, one legacy recorder,
// one router, one caller and one finite settings source. Captures cannot be swapped
// during invocation except the explicit fixed close-cancel probe in its closed
// mode. Closed controller channels are part of the test-only census;
// no production positive contract is created. Mutation tests change actual facts
// before validation only.
type auditFixtureMode uint8

const (
	auditFixtureAccept auditFixtureMode = iota
	auditFixtureError
	auditFixturePanic
	auditFixtureCancel
	auditFixtureNonAcceptance
	auditFixtureReenter
	auditFixtureDrain
	auditFixtureCallerCancel
	auditFixtureTimerOnly
	auditFixtureCompleteAt
	auditFixtureCompleteLate
	auditFixtureControlled
	auditFixtureActiveClose
)

// A closed caller graph: no parent, goroutine, real timer, arbitrary value
// provider or blocking wait. Only this fixture's Cancel can close its channel.
type auditFixtureCaller struct {
	done chan struct{}
	once sync.Once
}

func (c *auditFixtureCaller) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *auditFixtureCaller) Done() <-chan struct{}       { return c.done }
func (c *auditFixtureCaller) Value(any) any               { return nil }
func (c *auditFixtureCaller) Err() error {
	select {
	case <-c.done:
		return context.Canceled
	default:
		return nil
	}
}
func (c *auditFixtureCaller) Cancel() { c.once.Do(func() { close(c.done) }) }

// Every method panics: rejection must use live identity without invoking one.
// This hostile context owns no parent, wait, callback, timer or goroutine.
type auditHostileCaller struct{}

func (*auditHostileCaller) Deadline() (time.Time, bool) { panic("unproved caller Deadline invoked") }
func (*auditHostileCaller) Done() <-chan struct{}       { panic("unproved caller Done invoked") }
func (*auditHostileCaller) Value(any) any               { panic("unproved caller Value invoked") }
func (*auditHostileCaller) Err() error                  { panic("unproved caller Err invoked") }

type auditFixtureClock struct {
	tick         time.Duration
	epoch        uint64
	valid        bool
	timerAt      time.Duration
	cancel       context.CancelFunc
	timerStarts  int
	finalEntered chan struct{}
	finalRelease chan struct{}
}

func (c *auditFixtureClock) Read() decisionAuditClockReading {
	if c.finalEntered != nil {
		entered, release := c.finalEntered, c.finalRelease
		c.finalEntered, c.finalRelease = nil, nil
		close(entered)
		auditFixtureWait(release)
	}
	return decisionAuditClockReading{tick: c.tick, epoch: c.epoch, valid: c.valid}
}
func (c *auditFixtureClock) CancelAt(at time.Duration, cancel context.CancelFunc) func() {
	// The local reviewed scheduler has exactly one timer, no goroutine or queue.
	if c.cancel != nil {
		panic("fixture timer capacity exceeded")
	}
	c.timerStarts++
	c.timerAt, c.cancel = at, cancel
	return func() { c.cancel = nil }
}
func (c *auditFixtureClock) advance(tick time.Duration) {
	c.tick = tick
	if c.cancel != nil && tick >= c.timerAt {
		cancel := c.cancel
		c.cancel = nil
		cancel()
	}
}

type auditFixtureLegacy struct{ records []*store.DecisionAuditRecord }

func (l *auditFixtureLegacy) EmitDecisionAudit(_ context.Context, record *store.DecisionAuditRecord) {
	if len(l.records) >= 8 {
		panic("finite legacy fixture capacity exceeded")
	}
	l.records = append(l.records, record)
}

type auditFixtureHandler struct {
	mode                auditFixtureMode
	clock               *auditFixtureClock
	router              *decisionAuditRouter
	calls               int
	records             []store.DecisionAuditRecord
	returned            bool
	canceled            bool
	retained            *store.DecisionAuditRecord
	caller              *auditFixtureCaller
	callerCanceledOwned bool
	ready               chan struct{}
	release             chan struct{}
	cancelSeen          chan struct{}
	gateFree            bool
	boundaryOK          bool
	ownedSlot           *decisionAuditNewSlot
	closeProbe          *auditFixtureCancelProbe
}

// A fixed test-only probe wraps only this slot's already-owned cancel function.
// It observes the first synchronous close cancellation at the actual call site,
// then drops the original function; no parent, arbitrary callback or watcher.
type auditFixtureCancelProbe struct {
	router        *decisionAuditRouter
	cancel        context.CancelFunc
	calls         int
	firstGateFree bool
}

func (p *auditFixtureCancelProbe) Cancel() {
	if p.calls == 0 {
		p.firstGateFree = p.router.gate.TryLock()
		if p.firstGateFree {
			p.router.gate.Unlock()
		}
	}
	p.calls++
	if p.cancel != nil {
		cancel := p.cancel
		p.cancel = nil
		cancel()
	}
}

func (h *auditFixtureHandler) AcceptDecision(ctx context.Context, record *store.DecisionAuditRecord) (decisionAuditAcceptance, error) {
	h.calls++
	if h.calls > 8 {
		panic("finite handler invocation bound exceeded")
	}
	h.records = append(h.records, *record)
	h.retained = record
	defer func() { h.retained = nil; h.returned = true }()
	switch h.mode {
	case auditFixtureError:
		return decisionAuditAcceptance{}, errors.New("finite fixture error")
	case auditFixturePanic:
		panic("finite fixture panic")
	case auditFixtureCancel:
		return decisionAuditAcceptance{}, context.Canceled
	case auditFixtureCallerCancel:
		// Cancel the originating admitted caller after ownership, then accept.
		// This is distinct from returning a canceled error or firing the timer.
		h.callerCanceledOwned = h.router.inspect().active == decisionAuditNewCapacity && h.caller.Err() == nil
		h.caller.Cancel()
	case auditFixtureNonAcceptance:
		return decisionAuditAcceptance{}, nil
	case auditFixtureReenter:
		// A deterministic scheduler interleaving while the original slot is owned.
		// Bound recursion so a broken K check fails assertions rather than hangs.
		if h.calls == 1 {
			h.router.EmitDecisionAudit(h.caller, &store.DecisionAuditRecord{Result: "deny", ResourceType: "project"})
		}
	case auditFixtureTimerOnly:
		h.boundaryOK = h.clock.timerAt == h.clock.tick+decisionAuditCancelBudget
		h.clock.advance(h.clock.timerAt - time.Nanosecond)
		h.boundaryOK = h.boundaryOK && ctx.Err() == nil
		h.clock.advance(h.clock.timerAt)
		h.canceled = ctx.Err() == context.Canceled
	case auditFixtureCompleteAt, auditFixtureCompleteLate:
		// Completion clock and timer dispatch are separate closed virtual steps.
		// The independent timer-only case proves dispatch; this isolates return age.
		h.clock.tick += decisionAuditCompleteBudget
		if h.mode == auditFixtureCompleteLate {
			h.clock.tick += time.Nanosecond
		}
	case auditFixtureControlled, auditFixtureActiveClose:
		h.router.gate.Lock()
		h.ownedSlot = h.router.slot
		if h.mode == auditFixtureActiveClose {
			h.closeProbe = &auditFixtureCancelProbe{router: h.router, cancel: h.ownedSlot.cancel}
			h.ownedSlot.cancel = h.closeProbe.Cancel
		}
		h.router.gate.Unlock()
		close(h.ready)
		if h.mode == auditFixtureActiveClose {
			auditFixtureWait(ctx.Done())
			h.canceled = ctx.Err() == context.Canceled
			h.gateFree = h.router.gate.TryLock()
			if h.gateFree {
				h.router.gate.Unlock()
			}
			close(h.cancelSeen)
		}
		auditFixtureWait(h.release)
	case auditFixtureDrain:
		h.router.invalidateSource()
		h.clock.advance(h.clock.tick + decisionAuditCancelBudget)
		h.canceled = ctx.Err() != nil
	}
	return decisionAuditAcceptance{accepted: true}, nil
}

type auditFixture struct {
	router   *decisionAuditRouter
	handler  *auditFixtureHandler
	clock    *auditFixtureClock
	legacy   *auditFixtureLegacy
	contract decisionAuditLocalContract
	settings *auditFixtureSettingStore
	ops      *OperationalSettings
	caller   *auditFixtureCaller
}

func newAuditFixture(t *testing.T, mode auditFixtureMode) *auditFixture {
	t.Helper()
	clock := &auditFixtureClock{epoch: 1, valid: true}
	legacy := &auditFixtureLegacy{}
	srv := &Server{}
	// This registry exists only in the fixture. The real compiled registry test
	// independently demands the production default-false registration.
	reg, err := experiments.NewRegistry([]experiments.Experiment{{
		Name: experiments.AuthorizationDecisionAuditV2, Title: "Authorization decision audit v2",
		Description: "Finite test-domain decision routing only.", Layers: []experiments.Layer{experiments.LayerServer},
		Stage: experiments.StageAlpha, Issue: "ptone/scion#2379", Owner: "audit-update", ReviewBy: "2026-11-30",
	}}, nil)
	if err != nil {
		t.Fatalf("finite registry construction: %v", err)
	}
	srv.experiments = reg
	router := newDecisionAuditRouter(legacy, srv)
	srv.decisionAuditRouter = router
	caller := &auditFixtureCaller{done: make(chan struct{})}
	handler := &auditFixtureHandler{mode: mode, clock: clock, router: router, caller: caller, ready: make(chan struct{}), release: make(chan struct{}), cancelSeen: make(chan struct{})}
	manifest := decisionAuditManifestV1{
		version: decisionAuditManifestVersion, source: "fixture/591e24c", profile: fmt.Sprintf("finite-mode-%d", mode),
		root: "sampled-decision", signature: "EmitDecisionAudit(context.Context,*store.DecisionAuditRecord)",
		ratification: "Amendment14/finite-local-mechanics-only", generation: 1, clockEpoch: 1, expires: time.Hour,
		entries: []decisionAuditManifestEntry{
			{ordinal: 0, slot: "root", receiver: "decisionAuditRouter", target: "EmitDecisionAudit", signature: "context,record->void", capture: "legacy+handler+clock+generation+caller", contract: "exclusive-K1", instance: 1, generation: 1},
			{ordinal: 1, slot: "handler", receiver: "auditFixtureHandler", target: "AcceptDecision", signature: "context,record->acceptance,error", capture: fmt.Sprintf("mode-%d+clock+router+caller+three-owned-channels+one-slot+fixed-close-cancel-probe", mode), contract: "finite-sync-return-2s", instance: 2, generation: 1},
			{ordinal: 2, slot: "clock", receiver: "auditFixtureClock", target: "Read", signature: "->elapsed,epoch,valid", capture: "single-tick-epoch+one-final-read-barrier", contract: "injected-elapsed", instance: 3, generation: 1},
			{ordinal: 3, slot: "timer", receiver: "auditFixtureClock", target: "CancelAt", signature: "elapsed,cancel->stop", capture: "one-timer+context-cancel", contract: "finite-single-timer", instance: 3, generation: 1},
			{ordinal: 4, slot: "legacy", receiver: "auditFixtureLegacy", target: "EmitDecisionAudit", signature: "context,record->void", capture: "eight-record-array", contract: "finite-recorder", instance: 4, generation: 1},
			{ordinal: 5, slot: "settings", receiver: "auditFixtureSettingStore", target: "ListHubSettings", signature: "context->bounded-rows,error", capture: "129-rows+64KiB-plus-one+clock+closed-mode+router+caller+two-slot-prepanic-release+one-event-publisher", contract: "cooperative-read-1s", instance: 5, generation: 1},
			{ordinal: 6, slot: "caller", receiver: "auditFixtureCaller", target: "Err/Done/Deadline/Value/Cancel", signature: "bounded-context+cancel->canceled", capture: "one-close-channel+no-parent", contract: "finite-caller-cancellation", instance: 6, generation: 1},
		},
	}
	actual := manifest
	actual.entries = append([]decisionAuditManifestEntry(nil), manifest.entries...)
	contract := decisionAuditLocalContract{expected: manifest, actual: actual, clock: clock, handler: handler}
	f := &auditFixture{router: router, handler: handler, clock: clock, legacy: legacy, contract: contract, caller: caller}
	f.ops, f.settings = newAuditFixtureSettings(t, f)
	binding := decisionAuditLocalBinding{root: router, legacy: legacy, handler: handler, clock: clock, settings: f.ops, caller: caller}
	actualBinding := binding
	f.contract.expected.binding, f.contract.actual.binding = &binding, &actualBinding
	admission, _ := validateDecisionAuditManifest(f.contract)
	router.contract, router.admission = f.contract, admission
	return f
}

// Each observation uses the actual owned Refresh publication seam and the
// finite authoritative fixture. Sequence labels describe test ordering only;
// the observer itself must issue and validate its own sequence/attachment token.
func (f *auditFixture) observe(_ uint64, revision int64, enabled bool) decisionAuditRefreshObservation {
	f.settings.mu.Lock()
	f.settings.settings["experiments"].Value = json.RawMessage(fmt.Sprintf(`{"overrides":{"hub.authorization_decision_audit_v2":%t}}`, enabled))
	f.settings.settings["experiments"].Revision = revision
	f.settings.mu.Unlock()
	_, _ = f.ops.Refresh(context.Background())
	_, observation := f.ops.decisionAuditSnapshot()
	return observation
}
func (f *auditFixture) emit() {
	f.router.EmitDecisionAudit(f.caller, &store.DecisionAuditRecord{Result: "allow", ResourceType: "project", ResourceID: "finite-fixture"})
}
func (f *auditFixture) requireAdmission(t *testing.T) {
	t.Helper()
	if f.router.admission == nil {
		t.Fatal("complete finite fixture admission required (Stage A intentionally rejects)")
	}
}

// Five seconds is an infrastructure deadlock watchdog, not an audit timing proof.
// All contract deadlines below use the admitted virtual clock, never sleeps.
func auditFixtureWait(ch <-chan struct{}) {
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		panic("finite fixture handshake exceeded watchdog")
	}
}
func auditRequireReleased(t *testing.T, f *auditFixture, slot *decisionAuditNewSlot) {
	t.Helper()
	if f.router.inspect().active != 0 || f.router.slot != nil || f.clock.cancel != nil || f.handler.retained != nil {
		t.Fatal("slot/timer/handler reference retained after original completion")
	}
	if slot != nil {
		select {
		case <-slot.done:
		default:
			t.Fatal("original done accounting not completed")
		}
		if slot.record != nil || slot.caller != nil || slot.admission != nil || slot.handler != nil || slot.clock != nil || slot.cancel != nil {
			t.Fatal("completed slot retained graph references")
		}
	}
}

func TestDecisionAuditAdmission_ExactFiniteManifest(t *testing.T) {
	f := newAuditFixture(t, auditFixtureAccept)
	admission, rejection := validateDecisionAuditManifest(f.contract)
	if admission == nil {
		t.Errorf("exact finite manifest admission is nil/rejected (%s); want one nonempty admission", rejection)
		return
	}
	if len(admission.canonical) == 0 || admission.manifest.generation != 1 || len(admission.manifest.entries) != len(f.contract.expected.entries) {
		t.Fatal("admission must retain complete immutable canonical facts")
	}
	// Mutating constructor input must not mutate the admitted value.
	f.contract.actual.entries[0].target = "drift"
	f.contract.actual.binding.handler = &auditUnprovedHandler{}
	if admission.manifest.entries[0].target != "EmitDecisionAudit" || admission.manifest.binding == nil || admission.manifest.binding.handler != f.handler {
		t.Fatal("admission aliases mutable manifest input")
	}
}

func TestDecisionAuditAdmission_RejectsWholeManifest(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*decisionAuditLocalContract)
	}{
		{"missing", func(c *decisionAuditLocalContract) { c.actual.entries = c.actual.entries[:5] }},
		{"extra", func(c *decisionAuditLocalContract) { c.actual.entries = append(c.actual.entries, c.actual.entries[0]) }},
		{"duplicate", func(c *decisionAuditLocalContract) { c.actual.entries[1] = c.actual.entries[0] }},
		{"reorder", func(c *decisionAuditLocalContract) {
			c.actual.entries[0], c.actual.entries[1] = c.actual.entries[1], c.actual.entries[0]
		}},
		{"signature", func(c *decisionAuditLocalContract) { c.actual.entries[1].signature = "changed" }},
		{"receiver", func(c *decisionAuditLocalContract) { c.actual.entries[1].receiver = "changed" }},
		{"instance", func(c *decisionAuditLocalContract) { c.actual.entries[1].instance++ }},
		{"live-root", func(c *decisionAuditLocalContract) { c.actual.binding.root = nil }},
		{"live-clock", func(c *decisionAuditLocalContract) { c.actual.binding.clock = nil }},
		{"live-caller", func(c *decisionAuditLocalContract) {
			c.actual.binding.caller = &auditFixtureCaller{done: make(chan struct{})}
		}},
		{"live-settings", func(c *decisionAuditLocalContract) { c.actual.binding.settings = nil }},
		{"live-handler", func(c *decisionAuditLocalContract) { c.actual.binding.handler = &auditUnprovedHandler{} }},
		{"target", func(c *decisionAuditLocalContract) { c.actual.entries[1].target = "changed" }},
		{"capture", func(c *decisionAuditLocalContract) { c.actual.entries[1].capture = "changed" }},
		{"contract", func(c *decisionAuditLocalContract) { c.actual.entries[1].contract = "unknown" }},
		{"generation", func(c *decisionAuditLocalContract) { c.actual.generation++ }},
		{"version", func(c *decisionAuditLocalContract) { c.actual.version = "unknown" }},
		{"expired", func(c *decisionAuditLocalContract) { c.actual.expires = 0 }},
		{"unratified", func(c *decisionAuditLocalContract) { c.expected.ratification = "" }},
		{"too-large", func(c *decisionAuditLocalContract) {
			c.actual.entries[1].signature = strings.Repeat("x", decisionAuditManifestMaxBytes+1)
		}},
		{"collision", func(c *decisionAuditLocalContract) { c.actual.entries[1].slot = c.actual.entries[0].slot }},
		{"too-many", func(c *decisionAuditLocalContract) {
			for len(c.actual.entries) <= decisionAuditManifestMaxEntries {
				c.actual.entries = append(c.actual.entries, c.actual.entries[0])
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAuditFixture(t, auditFixtureAccept)
			tc.mutate(&f.contract)
			a, r := validateDecisionAuditManifest(f.contract)
			if a != nil || r == "" {
				t.Fatalf("whole rejection required, got %v/%q", a, r)
			}
		})
	}
	for _, extra := range []int{0, 1} {
		t.Run(fmt.Sprintf("isolated-byte-cap-plus-%d", extra), func(t *testing.T) {
			f := newAuditFixture(t, auditFixtureAccept)
			base, ok := canonicalDecisionAuditManifest(f.contract.expected)
			if !ok {
				t.Fatal("baseline canonical manifest required")
			}
			padding := strings.Repeat("x", decisionAuditManifestMaxBytes-len(base)+extra)
			f.contract.expected.entries[1].capture += padding
			f.contract.actual.entries[1].capture += padding
			// Both complete facts are identical; only aggregate canonical size differs.
			a, _ := validateDecisionAuditManifest(f.contract)
			if (a != nil) != (extra == 0) {
				t.Fatal("isolated canonical byte boundary mismatch")
			}
			if a != nil && len(a.canonical) != decisionAuditManifestMaxBytes {
				t.Fatal("accepted boundary not exact")
			}
		})
	}
	for _, count := range []int{7, 8} {
		t.Run(fmt.Sprintf("schema-cardinality-%d", count), func(t *testing.T) {
			f := newAuditFixture(t, auditFixtureAccept)
			if count == 8 {
				f.contract.expected.entries = append(f.contract.expected.entries, f.contract.expected.entries[0])
				f.contract.actual.entries = append(f.contract.actual.entries, f.contract.actual.entries[0])
			}
			a, _ := validateDecisionAuditManifest(f.contract)
			if (a != nil) != (count == 7) {
				t.Fatal("fixed seven-role cardinality mismatch")
			}
			// A26: this is 7/8 schema coverage, NOT unreachable 16/17 ceiling coverage.
		})
	}

}

// An unproved implementation is never invoked, including on rejection paths.
type auditUnprovedHandler struct{ calls int }

func (h *auditUnprovedHandler) AcceptDecision(context.Context, *store.DecisionAuditRecord) (decisionAuditAcceptance, error) {
	h.calls++
	panic("unproved handler invoked")
}
func TestDecisionAuditAdmission_RejectsUnprovedHandlerAndClock(t *testing.T) {
	for _, mode := range []string{"no-handler", "no-clock", "unknown-clock", "unknown-handler", "no-timer-proof", "no-read-proof", "no-return-proof"} {
		t.Run(mode, func(t *testing.T) {
			f := newAuditFixture(t, auditFixtureAccept)
			unknown := &auditUnprovedHandler{}
			switch mode {
			case "no-handler":
				f.contract.handler = nil
			case "no-clock":
				f.contract.clock = nil
			case "unknown-clock":
				f.clock.valid = false
			case "unknown-handler":
				f.contract.handler = unknown
			case "no-timer-proof":
				f.contract.actual.entries[3].contract = ""
			case "no-read-proof":
				f.contract.actual.entries[5].contract = ""
			case "no-return-proof":
				f.contract.actual.entries[1].contract = "context-ignoring-unbounded"
			}
			a, r := validateDecisionAuditManifest(f.contract)
			if a != nil || r == "" {
				t.Fatal("unproved contract accepted")
			}
			if unknown.calls != 0 {
				t.Fatal("unproved handler ran before rejection")
			}
		})
	}
}

func TestDecisionAuditAdmission_LeaseBoundaryAndEpoch(t *testing.T) {
	for _, mode := range []string{"before", "at", "after", "epoch", "regression", "unknown", "wall-metadata"} {
		t.Run(mode, func(t *testing.T) {
			f := newAuditFixture(t, auditFixtureAccept)
			f.requireAdmission(t)
			f.clock.tick = time.Second
			o := f.observe(1, 1, true)
			if !o.successful || o.deadline != time.Second+decisionAuditLease {
				t.Fatal("successful lease must anchor to q0")
			}
			f.clock.tick = o.deadline - time.Nanosecond
			want := 1
			switch mode {
			case "at":
				f.clock.tick = o.deadline
				want = 0
			case "after":
				f.clock.tick = o.deadline + time.Nanosecond
				want = 0
			case "epoch":
				f.clock.epoch++
				want = 0
			case "regression":
				f.clock.tick = 0
				want = 0
			case "unknown":
				f.clock.valid = false
				want = 0
			case "wall-metadata":
				o.snapshot.UpdatedAt = time.Unix(1<<30, 0)
				f.clock.tick = o.deadline
				want = 0
			}
			f.emit()
			if f.handler.calls != want || len(f.legacy.records) != 1-want {
				t.Fatal("lease/epoch handoff ownership mismatch")
			}
		})
	}
	if decisionAuditLease+decisionAuditLeaseSlack != 75*time.Second {
		t.Fatal("75-second total bound changed")
	}
}

func TestDecisionAuditRouter_LegacyRejectedStates(t *testing.T) {
	for _, mode := range []string{"off", "absent", "malformed", "unknown", "stale", "unhealthy", "unratified", "drift", "capacity", "unproved-caller", "pre-canceled-caller", "local-context"} {
		t.Run(mode, func(t *testing.T) {
			f := newAuditFixture(t, auditFixtureAccept)
			f.observe(1, 1, mode != "off")
			var caller context.Context = f.caller
			f.router.gate.Lock()
			switch mode {
			case "unproved-caller":
				caller = &auditHostileCaller{}
			case "pre-canceled-caller":
				f.caller.Cancel()
			case "local-context":
				local, cancel := context.WithCancel(context.Background())
				defer cancel()
				caller = local
			case "absent":
				f.router.state.observation.snapshot.Present = false
			case "malformed":
				f.router.state.observation.snapshot.Malformed = true
			case "unknown":
				f.router.state.observation.snapshot.Overrides = map[string]bool{"hub.unknown": true}
			case "stale":
				f.clock.tick = decisionAuditLease
			case "unhealthy":
				f.router.state.fault = true
			case "unratified":
				f.router.admission = nil
			case "drift":
				f.router.contract.actual.generation++
			case "capacity":
				f.router.state.active = decisionAuditNewCapacity
			}
			f.router.gate.Unlock()
			f.router.EmitDecisionAudit(caller, &store.DecisionAuditRecord{Result: "allow", ResourceType: "project", ResourceID: "finite-fixture"})
			if len(f.legacy.records) != 1 || f.handler.calls != 0 {
				t.Fatal("rejected record requires exactly one legacy owner")
			}
		})
	}
	for _, mode := range []string{"nil-record", "record-at-cap", "record-plus-one"} {
		t.Run(mode, func(t *testing.T) {
			f := newAuditFixture(t, auditFixtureAccept)
			f.requireAdmission(t)
			f.observe(1, 1, true)
			var record *store.DecisionAuditRecord
			want := 0
			if mode != "nil-record" {
				n := decisionAuditRecordMaxBytes - int(reflect.TypeOf(store.DecisionAuditRecord{}).Size())
				if mode == "record-plus-one" {
					n++
				} else {
					want = 1
				}
				record = &store.DecisionAuditRecord{ResourceID: strings.Repeat("r", n)}
			}
			frozen, ok := finiteDecisionAuditRecord(record)
			if ok != (want == 1) {
				t.Fatal("record cap not isolated before routing")
			}
			if ok && (frozen == record || frozen.ResourceID != record.ResourceID) {
				t.Fatal("record must be bounded copied value")
			}
			f.router.EmitDecisionAudit(f.caller, record)
			if f.handler.calls != want || len(f.legacy.records) != 1-want {
				t.Fatal("record boundary ownership mismatch")
			}
			auditRequireReleased(t, f, nil)
		})
	}

}

func TestDecisionAuditRouter_OneSynchronousNewOwner(t *testing.T) {
	f := newAuditFixture(t, auditFixtureAccept)
	f.observe(1, 1, true)
	authz := &AuthzService{DecisionAuditSampleRate: 1}
	authz.SetDecisionAuditEmitter(f.router)
	request := AuthzRequest{Resource: Resource{Type: "project", ID: "finite-fixture"}, Action: ActionRead}
	decision := Decision{Allowed: true, Reason: "finite authorization result"}
	authz.emitDecisionAudit(f.caller, request, decision)
	if !decision.Allowed || decision.Reason != "finite authorization result" {
		t.Fatal("audit changed authorization result")
	}
	if f.handler.calls+len(f.legacy.records) != 1 {
		t.Fatal("sampled decision must have exactly one owner")
	}
	if len(f.legacy.records) == 1 && f.legacy.records[0].Result != "allow" {
		t.Fatal("legacy result mapping changed")
	}
	if f.handler.calls != 1 {
		t.Errorf("NEW count=%d, want 1; legacy count=%d (Stage A expected 1)", f.handler.calls, len(f.legacy.records))
		return
	}
	if len(f.legacy.records) != 0 || !f.handler.returned || f.handler.retained != nil || f.router.inspect().active != 0 {
		t.Fatal("NEW must return synchronously, release references, and exclude legacy")
	}
}

func TestDecisionAuditRouter_NewFailureNoFallback(t *testing.T) {
	for _, mode := range []auditFixtureMode{auditFixtureError, auditFixturePanic, auditFixtureCancel, auditFixtureNonAcceptance, auditFixtureCallerCancel} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			f := newAuditFixture(t, mode)
			f.requireAdmission(t)
			f.observe(1, 1, true)
			f.emit()
			state := f.router.inspect()
			if mode == auditFixtureCallerCancel && (!f.handler.callerCanceledOwned || f.caller.Err() != context.Canceled || !f.handler.returned || f.handler.retained != nil) {
				t.Fatal("originating caller must cancel after NEW ownership, accept, and release references")
			}
			if f.handler.calls != 1 || len(f.legacy.records) != 0 || !state.fault || state.failures != 1 || state.active != 0 {
				t.Fatal("NEW fault must latch once without same-record fallback")
			}
			f.observe(2, 1, true)
			f.emit()
			if f.handler.calls != 1 || len(f.legacy.records) != 1 || f.router.inspect().failures != 1 {
				t.Fatal("next decision must be legacy without re-arm")
			}
		})
	}
}

func TestDecisionAuditRouter_ConcurrentOwnershipAndCapacity(t *testing.T) {
	// Deterministic reentrant scheduler models the second owner transaction during
	// a live K=1 slot without scheduler timing, goroutines or an unbounded wait.
	f := newAuditFixture(t, auditFixtureReenter)
	f.requireAdmission(t)
	f.observe(1, 1, true)
	f.emit()
	if f.handler.calls != 1 || len(f.legacy.records) != 1 || f.router.inspect().active != 0 {
		t.Fatal("occupied K=1 must route competing decision to legacy without waiting")
	}
	auditRequireReleased(t, f, nil)
	// Reuse the same healthy slot with exact caller: no fault, timer overwrite or
	// permanent capacity leak is permitted after the reentrant owner returned.
	f.emit()
	if f.handler.calls != 2 || len(f.legacy.records) != 1 {
		t.Fatal("released K=1 slot must be reusable")
	}
	t.Run("controlled-final-gate-handoff", func(t *testing.T) {
		g := newAuditFixture(t, auditFixtureControlled)
		g.requireAdmission(t)
		g.observe(1, 1, true)
		entered, release := make(chan struct{}), make(chan struct{})
		g.clock.finalEntered, g.clock.finalRelease = entered, release
		firstDone, secondDone := make(chan struct{}), make(chan struct{})
		go func() { defer close(firstDone); g.emit() }()
		auditFixtureWait(entered) // first candidate reached actual final-gate clock read
		started := make(chan struct{})
		go func() { close(started); defer close(secondDone); g.emit() }()
		auditFixtureWait(started)
		close(release)
		auditFixtureWait(g.handler.ready)
		auditFixtureWait(secondDone) // admitted first owner remains live until release
		original := g.handler.ownedSlot
		g.router.gate.Lock()
		same := g.router.slot == original && g.router.state.active == decisionAuditNewCapacity
		g.router.gate.Unlock()
		if !same || g.handler.calls != 1 || len(g.legacy.records) != 1 || g.clock.cancel == nil || g.clock.timerAt != original.handoff+decisionAuditCancelBudget {
			t.Error("competing handoff overwrote live K=1 slot/timer or ownership")
		}
		close(g.handler.release)
		auditFixtureWait(firstDone)
		auditRequireReleased(t, g, original)
		// Reuse controlled mode with fresh owned channels, no changed admission.
		g.handler.ready = make(chan struct{})
		g.handler.release = make(chan struct{})
		close(g.handler.release)
		g.emit()
		if g.handler.calls != 2 || len(g.legacy.records) != 1 {
			t.Fatal("controlled handoff slot cannot be reused")
		}
		auditRequireReleased(t, g, g.handler.ownedSlot)
	})
	old := f.observe(2, 1, true)
	f.router.invalidateSource()
	f.router.finishRefresh(old, old.snapshot, nil)
	f.emit()
	if f.handler.calls != 2 || len(f.legacy.records) != 2 {
		t.Fatal("obsolete observation cannot resurrect NEW after invalidation")
	}
}

func TestDecisionAuditRouter_DisableDrainsCooperativeSlots(t *testing.T) {
	f := newAuditFixture(t, auditFixtureDrain)
	f.requireAdmission(t)
	f.observe(1, 1, true)
	h := f.clock.tick
	f.emit()
	if !f.handler.canceled || !f.handler.returned || f.handler.retained != nil || f.clock.tick > h+decisionAuditCompleteBudget || f.router.inspect().active != 0 {
		t.Fatal("h-based finite cancel/complete/reference-release bound violated")
	}
	_ = f.router.CloseNew(context.Background())
	f.emit()
	if len(f.legacy.records) != 1 {
		t.Fatal("after NEW close, one legacy owner required")
	}
	for _, mode := range []auditFixtureMode{auditFixtureTimerOnly, auditFixtureCompleteAt, auditFixtureCompleteLate} {
		t.Run(fmt.Sprintf("independent-bound-%d", mode), func(t *testing.T) {
			g := newAuditFixture(t, mode)
			g.requireAdmission(t)
			g.observe(1, 1, true)
			h := g.clock.tick
			g.emit()
			state := g.router.inspect()
			if g.clock.timerAt != h+decisionAuditCancelBudget {
				t.Fatal("timer must schedule exact h+1")
			}
			if mode == auditFixtureTimerOnly && (!g.handler.boundaryOK || !g.handler.canceled) {
				t.Fatal("timer alone must cancel at h+1, never before")
			}
			wantFault := mode != auditFixtureCompleteAt
			if state.fault != wantFault || state.failures != map[bool]int{false: 0, true: 1}[wantFault] || g.handler.calls != 1 || len(g.legacy.records) != 0 {
				t.Fatal("completion boundary must isolate one fault without same-record fallback")
			}
			auditRequireReleased(t, g, nil)
			if wantFault {
				g.emit()
				if g.handler.calls != 1 || len(g.legacy.records) != 1 {
					t.Fatal("next record after finite failure must be legacy")
				}
			}
		})
	}

}

func TestDecisionAuditRouter_CloseNewPreservesFallback(t *testing.T) {
	for _, mode := range []string{"close-new", "server-cleanup", "server-http-drain"} {
		t.Run(mode, func(t *testing.T) {
			f := newAuditFixture(t, auditFixtureActiveClose)
			f.requireAdmission(t)
			f.observe(1, 1, true)
			emitDone, closeDone := make(chan struct{}), make(chan struct{})
			go func() { defer close(emitDone); f.emit() }()
			auditFixtureWait(f.handler.ready)
			original := f.handler.ownedSlot
			var httpEntered, httpRelease, requestDone, drainStarted chan struct{}
			var httpServer *httptest.Server
			var requestErr error
			if mode == "server-http-drain" {
				httpEntered, httpRelease, requestDone, drainStarted = make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
				httpServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					close(httpEntered)
					auditFixtureWait(httpRelease)
					f.emit()
					w.WriteHeader(http.StatusNoContent)
				}))
				httpServer.Config.RegisterOnShutdown(func() { close(drainStarted) })
				f.router.server.httpServer = httpServer.Config
				go func() {
					defer close(requestDone)
					client := &http.Client{Timeout: 5 * time.Second}
					response, err := client.Get(httpServer.URL)
					requestErr = err
					if err == nil {
						requestErr = response.Body.Close()
					}
				}()
				auditFixtureWait(httpEntered)
			}
			go func() {
				defer close(closeDone)
				switch mode {
				case "close-new":
					_ = f.router.CloseNew(context.Background())
				case "server-cleanup":
					_ = f.router.server.CleanupResources(context.Background())
				case "server-http-drain":
					_ = f.router.server.Shutdown(context.Background())
				}
			}()
			auditFixtureWait(f.handler.cancelSeen)
			if !f.handler.canceled || !f.handler.gateFree || f.handler.closeProbe.calls != 1 || !f.handler.closeProbe.firstGateFree || f.handler.closeProbe.cancel != nil || !f.router.inspect().closed || f.router.inspect().active != 1 {
				t.Error("live close must bar NEW and cancel outside gate while original owner remains")
			}
			select {
			case <-closeDone:
				t.Error("close returned before original owner accounting")
			default:
			}
			if drainStarted != nil {
				select {
				case <-drainStarted:
					t.Error("HTTP drain started before NEW completion")
				default:
				}
			}
			// Closed admission routes a bounded exact-caller record to legacy during drain.
			f.emit()
			if f.handler.calls != 1 || len(f.legacy.records) != 1 || f.router.slot != original {
				t.Error("close must retain original owner and legacy admission")
			}
			f.clock.tick = original.handoff + decisionAuditCompleteBudget
			close(f.handler.release)
			auditFixtureWait(emitDone)
			auditRequireReleased(t, f, original)
			state := f.router.inspect()
			if !f.handler.returned || f.clock.tick != original.handoff+decisionAuditCompleteBudget || !state.fault || state.failures != 1 {
				t.Error("canceled live owner must finish at original h+2 with one accounted fault")
			}
			if drainStarted != nil {
				auditFixtureWait(drainStarted)
				close(httpRelease)
				auditFixtureWait(requestDone)
				if requestErr != nil {
					t.Error("finite HTTP drain request failed", requestErr)
				}
			}
			auditFixtureWait(closeDone)
			if httpServer != nil {
				httpServer.Close()
				if len(f.legacy.records) != 2 {
					t.Fatal("HTTP request finishing during drain lost legacy routing")
				}
			}
			_ = f.router.CloseNew(context.Background())
			f.emit()
			want := 2
			if httpServer != nil {
				want = 3
			}
			if len(f.legacy.records) != want || f.handler.calls != 1 {
				t.Fatal("idempotent close must preserve legacy")
			}
		})
	}
	// Close adds no timeout; it waits for the original handoff-relative bounds.
	if decisionAuditCancelBudget != time.Second || decisionAuditCompleteBudget != 2*time.Second {
		t.Fatal("NEW handoff cancel/complete budgets changed")
	}
}
