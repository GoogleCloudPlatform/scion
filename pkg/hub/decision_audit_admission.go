// Copyright 2026 Google LLC
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"bytes"
	"context"
	"encoding/binary"
	"maps"
	"reflect"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	decisionAuditManifestVersion    = "audit-update.local-decision-routing/v1"
	decisionAuditNewCapacity        = 1
	decisionAuditManifestMaxEntries = 16
	decisionAuditManifestMaxBytes   = 16 << 10
	decisionAuditRecordMaxBytes     = 8 << 10
	decisionAuditSettingsMaxRows    = 128
	decisionAuditSettingsMaxBytes   = 64 << 10
	decisionAuditRefreshBudget      = time.Second
	decisionAuditLease              = 74 * time.Second
	decisionAuditLeaseSlack         = time.Second
	decisionAuditCancelBudget       = time.Second
	decisionAuditCompleteBudget     = 2 * time.Second
	decisionAuditNewHealthKey       = "authorization_decision_audit_new"
	decisionAuditLegacyHealthKey    = "authorization_decision_audit_legacy"
	decisionAuditFaultWarning       = "unhealthy: CRITICAL authorization decision logging fault; NEW off; triggering record may be lost; subsequent decisions use legacy"
)

// These facts describe a finite local contract, not production provenance.
// There is no production constructor for a positive contract or admission.
type decisionAuditManifestEntry struct {
	ordinal                                              uint16
	slot, receiver, target, signature, capture, contract string
	instance, generation                                 uint64
}

// Live references represent facts for comparison alongside canonical identifiers;
// numeric instance labels or receiver names alone cannot bind an actual target.
// No production constructor creates this binding. Fixture construction does.
type decisionAuditLocalBinding struct {
	root     *decisionAuditRouter
	legacy   DecisionAuditEmitter
	handler  decisionAuditLocalHandler
	clock    decisionAuditElapsedClock
	settings *OperationalSettings
	caller   context.Context
}

type decisionAuditManifestV1 struct {
	binding                                                 *decisionAuditLocalBinding
	version, source, profile, root, signature, ratification string
	generation, clockEpoch                                  uint64
	expires                                                 time.Duration
	entries                                                 []decisionAuditManifestEntry
}

type decisionAuditClockReading struct {
	tick  time.Duration
	epoch uint64
	valid bool
}

// No time.Now adapter is provided: suspension and timer bounds are unproved.
// A positive implementation and its finite timer graph exist only in tests.
type decisionAuditElapsedClock interface {
	Read() decisionAuditClockReading
	CancelAt(time.Duration, context.CancelFunc) func()
}

type decisionAuditAcceptance struct{ accepted bool }

type decisionAuditLocalHandler interface {
	AcceptDecision(context.Context, *store.DecisionAuditRecord) (decisionAuditAcceptance, error)
}

type decisionAuditLocalContract struct {
	expected, actual decisionAuditManifestV1
	clock            decisionAuditElapsedClock
	handler          decisionAuditLocalHandler
}

type decisionAuditAdmission struct {
	manifest  decisionAuditManifestV1
	canonical []byte
}

type decisionAuditRejection string

const decisionAuditUnratified decisionAuditRejection = "unratified"

// expected is the private, externally reviewed constructor census, created only
// in _test.go. It is not an admin document, self-asserted approval flag or an
// interface/name whitelist. Production has no producer of this authority value.
// Validation compares its whole graph AND exact bound live references before
// invoking even the clock. A caller supplying arbitrary expected facts is outside
// that closed constructor boundary and must never be wired into production.
func validateDecisionAuditManifest(c decisionAuditLocalContract) (*decisionAuditAdmission, decisionAuditRejection) {
	expected, ok := canonicalDecisionAuditManifest(c.expected)
	if !ok {
		return nil, decisionAuditUnratified
	}
	actual, ok := canonicalDecisionAuditManifest(c.actual)
	if !ok || !bytes.Equal(expected, actual) || !sameDecisionAuditBinding(c.expected.binding, c.actual.binding) ||
		!sameDecisionAuditReference(c.clock, c.expected.binding.clock) ||
		!sameDecisionAuditReference(c.handler, c.expected.binding.handler) {
		return nil, decisionAuditUnratified
	}
	reading := readDecisionAuditClock(c.clock)
	if !reading.valid || reading.epoch != c.expected.clockEpoch || reading.tick < 0 || reading.tick >= c.expected.expires {
		return nil, decisionAuditUnratified
	}
	m := c.expected
	m.entries = append([]decisionAuditManifestEntry(nil), m.entries...)
	binding := *m.binding
	m.binding = &binding
	return &decisionAuditAdmission{manifest: m, canonical: append([]byte(nil), expected...)}, ""
}

func sameDecisionAuditReference(a, b any) bool {
	if a == nil || b == nil {
		return false
	}
	av, bv := reflect.ValueOf(a), reflect.ValueOf(b)
	// Reference identity, not a receiver name or reflected method lookup.
	return av.Kind() == reflect.Pointer && bv.Kind() == reflect.Pointer && !av.IsNil() && !bv.IsNil() &&
		av.Type() == bv.Type() && av.Pointer() == bv.Pointer()
}

func sameDecisionAuditBinding(a, b *decisionAuditLocalBinding) bool {
	return a != nil && b != nil && a.root != nil && a.root == b.root &&
		a.settings != nil && a.settings == b.settings &&
		sameDecisionAuditReference(a.legacy, b.legacy) &&
		sameDecisionAuditReference(a.handler, b.handler) && sameDecisionAuditReference(a.clock, b.clock) &&
		sameDecisionAuditReference(a.caller, b.caller)
}

func canonicalDecisionAuditManifest(m decisionAuditManifestV1) ([]byte, bool) {
	// These are schema roles/proof protocol versions, not a production receiver
	// whitelist. Positive constructor facts/reference authority remain test-only.
	roles := [7]struct{ slot, target, signature, contract string }{
		{"root", "EmitDecisionAudit", "context,record->void", "exclusive-K1"},
		{"handler", "AcceptDecision", "context,record->acceptance,error", "finite-sync-return-2s"},
		{"clock", "Read", "->elapsed,epoch,valid", "injected-elapsed"},
		{"timer", "CancelAt", "elapsed,cancel->stop", "finite-single-timer"},
		{"legacy", "EmitDecisionAudit", "context,record->void", "finite-recorder"},
		{"settings", "ListHubSettings", "context->bounded-rows,error", "cooperative-read-1s"},
		{"caller", "Err/Done/Deadline/Value/Cancel", "bounded-context+cancel->canceled", "finite-caller-cancellation"},
	}
	if m.version != decisionAuditManifestVersion || m.generation == 0 || m.clockEpoch == 0 || m.expires <= 0 ||
		len(m.entries) != len(roles) || len(m.entries) > decisionAuditManifestMaxEntries || m.binding == nil ||
		m.binding.root == nil || m.binding.root.server == nil || m.binding.settings == nil ||
		!sameDecisionAuditReference(m.binding.handler, m.binding.handler) ||
		!sameDecisionAuditReference(m.binding.clock, m.binding.clock) ||
		!sameDecisionAuditReference(m.binding.legacy, m.binding.legacy) ||
		!sameDecisionAuditReference(m.binding.caller, m.binding.caller) {
		return nil, false
	}
	out := make([]byte, 0, 512)
	put := func(v string) bool {
		if len(v) == 0 || len(v) > decisionAuditManifestMaxBytes || len(out) > decisionAuditManifestMaxBytes-4-len(v) || !utf8.ValidString(v) {
			return false
		}
		out = binary.BigEndian.AppendUint32(out, uint32(len(v)))
		out = append(out, v...)
		return true
	}
	for _, v := range []string{m.version, m.source, m.profile, m.root, m.signature, m.ratification} {
		if !put(v) {
			return nil, false
		}
	}
	out = binary.BigEndian.AppendUint64(out, m.generation)
	out = binary.BigEndian.AppendUint64(out, m.clockEpoch)
	out = binary.BigEndian.AppendUint64(out, uint64(m.expires))
	out = binary.BigEndian.AppendUint32(out, uint32(len(m.entries)))
	for i, e := range m.entries {
		role := roles[i]
		if e.ordinal != uint16(i) || e.slot != role.slot || e.target != role.target || e.signature != role.signature || e.contract != role.contract || e.instance == 0 || e.generation != m.generation {
			return nil, false
		}
		for j := 0; j < i; j++ {
			prev := m.entries[j]
			if prev.slot == e.slot || (prev.receiver == e.receiver && prev.instance == e.instance && prev.target == e.target) {
				return nil, false
			}
		}
		out = binary.BigEndian.AppendUint16(out, e.ordinal)
		for _, v := range []string{e.slot, e.receiver, e.target, e.signature, e.capture, e.contract} {
			if !put(v) {
				return nil, false
			}
		}
		out = binary.BigEndian.AppendUint64(out, e.instance)
		out = binary.BigEndian.AppendUint64(out, e.generation)
		if len(out) > decisionAuditManifestMaxBytes {
			return nil, false
		}
	}
	return out, true
}

func readDecisionAuditClock(clock decisionAuditElapsedClock) (reading decisionAuditClockReading) {
	defer func() {
		if recover() != nil {
			reading = decisionAuditClockReading{}
		}
	}()
	if clock != nil {
		reading = clock.Read()
	}
	return reading
}

type decisionAuditRefreshObservation struct {
	source                           *OperationalSettings
	attachment, sequence, generation uint64
	q0, deadline                     time.Duration
	epoch                            uint64
	snapshot                         ExperimentsSnapshot
	successful                       bool
	tracked                          bool
}

type decisionAuditRouterState struct {
	observation      decisionAuditRefreshObservation
	closed, fault    bool
	active, failures int
}

// Production constructs only legacy/server references. No production setter,
// manifest factory, target substitution or reset API can populate admission.
type decisionAuditRouter struct {
	gate                          sync.Mutex
	legacy                        DecisionAuditEmitter
	server                        *Server
	contract                      decisionAuditLocalContract
	admission                     *decisionAuditAdmission
	state                         decisionAuditRouterState
	source                        *OperationalSettings
	attachment, sequence          uint64
	refreshSlots                  [2]decisionAuditRefreshObservation
	refreshOverlap, refreshPoison bool
	propagationLost               bool
	mutations                     uint8
	lastTick                      time.Duration
	clockInvalid                  bool
	maxRevision                   int64
	slot                          *decisionAuditNewSlot
}

type decisionAuditNewSlot struct {
	cancel    context.CancelFunc
	done      chan struct{}
	handoff   time.Duration
	admission *decisionAuditAdmission
	handler   decisionAuditLocalHandler
	clock     decisionAuditElapsedClock
	record    *store.DecisionAuditRecord
	caller    context.Context
}

var _ DecisionAuditEmitter = (*decisionAuditRouter)(nil)

func newDecisionAuditRouter(legacy DecisionAuditEmitter, server *Server) *decisionAuditRouter {
	return &decisionAuditRouter{legacy: legacy, server: server}
}

func (r *decisionAuditRouter) contractMatchesLocked() bool {
	a := r.admission
	if a == nil || a.manifest.binding == nil || a.manifest.binding.root != r ||
		!sameDecisionAuditReference(a.manifest.binding.legacy, r.legacy) ||
		!sameDecisionAuditReference(a.manifest.binding.clock, r.contract.clock) ||
		!sameDecisionAuditReference(a.manifest.binding.handler, r.contract.handler) ||
		!sameDecisionAuditBinding(a.manifest.binding, r.contract.expected.binding) ||
		!sameDecisionAuditBinding(a.manifest.binding, r.contract.actual.binding) {
		return false
	}
	expected, ok := canonicalDecisionAuditManifest(r.contract.expected)
	if !ok || !bytes.Equal(expected, a.canonical) {
		return false
	}
	actual, ok := canonicalDecisionAuditManifest(r.contract.actual)
	return ok && bytes.Equal(actual, a.canonical)
}

func (r *decisionAuditRouter) sourceMatchesLocked(source *OperationalSettings) bool {
	return source != nil && source == r.source && r.server != nil && r.server.GetOperationalSettings() == source &&
		r.contractMatchesLocked() && r.admission.manifest.binding.settings == source
}

func (r *decisionAuditRouter) clockLocked() (decisionAuditClockReading, bool) {
	if r.clockInvalid || !r.contractMatchesLocked() {
		return decisionAuditClockReading{}, false
	}
	c := readDecisionAuditClock(r.admission.manifest.binding.clock)
	// The epoch/tick never comes from wall time or store metadata. A clock
	// anomaly cannot re-arm this generation even if the next read looks valid.
	if !c.valid || c.epoch != r.admission.manifest.clockEpoch || c.tick < r.lastTick || c.tick < 0 ||
		c.tick >= r.admission.manifest.expires || c.tick > time.Duration(1<<63-1)-decisionAuditLease-decisionAuditCompleteBudget {
		r.clockInvalid = true
		return c, false
	}
	r.lastTick = c.tick
	return c, true
}

func boundedDecisionAuditSnapshot(s ExperimentsSnapshot) bool {
	if !s.Present || s.Malformed || s.Revision <= 0 || len(s.Overrides) > decisionAuditSettingsMaxRows {
		return false
	}
	size := 0
	for name := range s.Overrides {
		if len(name) > decisionAuditSettingsMaxBytes-size {
			return false
		}
		size += len(name)
	}
	return true
}

func cloneDecisionAuditSnapshot(s ExperimentsSnapshot) ExperimentsSnapshot {
	s.Overrides = maps.Clone(s.Overrides)
	return s
}
func sameDecisionAuditSnapshot(a, b ExperimentsSnapshot) bool {
	return boundedDecisionAuditSnapshot(a) && boundedDecisionAuditSnapshot(b) &&
		a.Present == b.Present && a.Malformed == b.Malformed && a.Revision == b.Revision && maps.Equal(a.Overrides, b.Overrides)
}

func (r *decisionAuditRouter) nextSequenceLocked() bool {
	if r.sequence == ^uint64(0) {
		r.refreshPoison = true
		r.state.observation = decisionAuditRefreshObservation{}
		return false
	}
	r.sequence++
	return true
}

func (r *decisionAuditRouter) beginRefresh(source *OperationalSettings) decisionAuditRefreshObservation {
	return r.beginRefreshForAttachment(source, nil)
}

func (r *decisionAuditRouter) beginRefreshForAttachment(source *OperationalSettings, expected *uint64) decisionAuditRefreshObservation {
	r.gate.Lock()
	defer r.gate.Unlock()
	if expected != nil && *expected != r.attachment {
		return decisionAuditRefreshObservation{source: source, attachment: *expected}
	}
	rejected := decisionAuditRefreshObservation{source: source, attachment: r.attachment}
	if !r.sourceMatchesLocked(source) || r.state.closed || r.state.fault || r.mutations != 0 || r.refreshPoison || r.propagationLost {
		return rejected
	}
	reading, ok := r.clockLocked()
	if !ok {
		r.state.observation = decisionAuditRefreshObservation{}
		return rejected
	}
	if !r.nextSequenceLocked() {
		return rejected
	}
	free := -1
	occupied := false
	for i, token := range r.refreshSlots {
		if token.tracked {
			occupied = true
		} else if free < 0 {
			free = i
		}
	}
	r.state.observation = decisionAuditRefreshObservation{}
	if free < 0 {
		r.refreshPoison = true
		return rejected
	}
	if occupied {
		r.refreshOverlap = true
	}
	token := decisionAuditRefreshObservation{source: source, attachment: r.attachment, sequence: r.sequence, generation: r.admission.manifest.generation, q0: reading.tick, epoch: reading.epoch, tracked: true}
	r.refreshSlots[free] = token
	return token
}

func (r *decisionAuditRouter) finishRefresh(token decisionAuditRefreshObservation, snapshot ExperimentsSnapshot, err error) decisionAuditRefreshObservation {
	result, _ := r.finishRefreshForAttachment(token, snapshot, err)
	return result
}

func (r *decisionAuditRouter) finishRefreshForAttachment(token decisionAuditRefreshObservation, snapshot ExperimentsSnapshot, err error) (decisionAuditRefreshObservation, bool) {
	r.gate.Lock()
	active := false
	for i, pending := range r.refreshSlots {
		if token.tracked && pending.tracked && pending.sequence == token.sequence && pending.attachment == token.attachment && pending.source == token.source {
			r.refreshSlots[i] = decisionAuditRefreshObservation{}
			active = true
			break
		}
	}
	overlapping := r.refreshOverlap
	empty := true
	for _, pending := range r.refreshSlots {
		if pending.tracked {
			empty = false
		}
	}
	if empty {
		r.refreshOverlap = false
	}
	// Old sources may release their own finite bookkeeping, never publish or
	// invalidate another attached source's freshly accepted state.
	if token.source != r.source || token.attachment != r.attachment {
		r.gate.Unlock()
		return decisionAuditRefreshObservation{}, false
	}
	reading, clockOK := r.clockLocked()
	valid := active && !overlapping && !r.refreshPoison && !r.propagationLost && err == nil && clockOK && !r.state.closed && !r.state.fault && r.mutations == 0 &&
		r.sourceMatchesLocked(token.source) && token.sequence == r.sequence && token.generation == r.admission.manifest.generation &&
		token.epoch == reading.epoch && reading.tick >= token.q0 && reading.tick-token.q0 <= decisionAuditRefreshBudget &&
		reading.tick < token.q0+decisionAuditLease && boundedDecisionAuditSnapshot(snapshot) && snapshot.Revision >= r.maxRevision &&
		snapshot.Overrides[experiments.AuthorizationDecisionAuditV2] && r.server.experimentEnabledIn(snapshot, experiments.AuthorizationDecisionAuditV2)
	if !valid {
		r.state.observation = decisionAuditRefreshObservation{}
		var cancel context.CancelFunc
		if r.slot != nil {
			cancel = r.slot.cancel
		}
		r.gate.Unlock()
		if cancel != nil {
			cancel()
		}
		return decisionAuditRefreshObservation{}, true
	}
	token.tracked = false
	token.successful = true
	token.deadline = token.q0 + decisionAuditLease
	token.snapshot = cloneDecisionAuditSnapshot(snapshot)
	r.maxRevision = snapshot.Revision
	r.state.observation = token
	result := token
	result.snapshot = cloneDecisionAuditSnapshot(snapshot)
	r.gate.Unlock()
	return result, true
}

// Source pointer publication shares the handoff gate. No cache mutex is taken.
func (r *decisionAuditRouter) setSource(source *OperationalSettings) {
	r.gate.Lock()
	old := r.source
	r.source = source
	if r.attachment == ^uint64(0) {
		r.refreshPoison = true
	} else {
		r.attachment++
		r.propagationLost = false // only a new explicit attachment releases lifecycle loss
	}
	r.nextSequenceLocked()
	r.state.observation = decisionAuditRefreshObservation{}
	r.server.operationalSettings.Store(source)
	var cancel context.CancelFunc
	if r.slot != nil {
		cancel = r.slot.cancel
	}
	r.gate.Unlock()
	if cancel != nil {
		cancel()
	}
	if old != nil && old != source {
		old.decisionAuditObserver.CompareAndSwap(r, nil)
	}
	if source != nil {
		source.decisionAuditObserver.Store(r)
	}
}

// Attachment identity is captured before propagation work starts. No cache lock
// is held here and no clock, handler or context method is invoked.
func (r *decisionAuditRouter) propagationAttachment(source *OperationalSettings) uint64 {
	r.gate.Lock()
	defer r.gate.Unlock()
	if source != r.source {
		return 0
	}
	return r.attachment
}

// Called with the source cache mutex held, matching Refresh's cache -> gate
// order. Return cancellation for invocation after BOTH locks are released.
func (r *decisionAuditRouter) losePropagation(source *OperationalSettings, attachment uint64) (context.CancelFunc, bool) {
	r.gate.Lock()
	defer r.gate.Unlock()
	if source != r.source || attachment != r.attachment {
		return nil, false
	}
	r.propagationLost = true
	r.nextSequenceLocked()
	r.state.observation = decisionAuditRefreshObservation{}
	if r.slot != nil {
		return r.slot.cancel, true
	}
	return nil, true
}

func (r *decisionAuditRouter) invalidateSource() { r.invalidateSettings(nil) }
func (r *decisionAuditRouter) invalidateSettings(source *OperationalSettings) {
	r.gate.Lock()
	if source != nil && source != r.source {
		r.gate.Unlock()
		return
	}
	r.nextSequenceLocked()
	r.state.observation = decisionAuditRefreshObservation{}
	var cancel context.CancelFunc
	if r.slot != nil {
		cancel = r.slot.cancel
	}
	r.gate.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (r *decisionAuditRouter) mutation(source *OperationalSettings, begin bool) {
	r.gate.Lock()
	if source != r.source {
		r.gate.Unlock()
		return
	}
	if begin {
		if r.mutations == 2 {
			r.refreshPoison = true
		} else {
			r.mutations++
		}
	} else if r.mutations != 0 {
		r.mutations--
	}
	r.nextSequenceLocked()
	r.state.observation = decisionAuditRefreshObservation{}
	var cancel context.CancelFunc
	if r.slot != nil {
		cancel = r.slot.cancel
	}
	r.gate.Unlock()
	if cancel != nil {
		cancel()
	}
}

func finiteDecisionAuditRecord(record *store.DecisionAuditRecord) (*store.DecisionAuditRecord, bool) {
	if record == nil {
		return nil, false
	}
	// No JSON/custom marshaler, method call, recursive graph walk or sink effect.
	// Strings are immutable; time.Time is copied as opaque, fixed-size metadata.
	value := reflect.ValueOf(*record)
	size := int(value.Type().Size())
	if size > decisionAuditRecordMaxBytes {
		return nil, false
	}
	for i := 0; i < value.NumField(); i++ {
		field := value.Field(i)
		switch field.Kind() {
		case reflect.String:
			n := field.Len()
			if n > decisionAuditRecordMaxBytes-size {
				return nil, false
			}
			size += n
		case reflect.Bool, reflect.Int, reflect.Int64, reflect.Uint64, reflect.Float64:
		case reflect.Struct:
			if field.Type() != reflect.TypeOf(time.Time{}) {
				return nil, false
			}
		case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Interface:
			if !field.IsNil() {
				return nil, false
			}
		default:
			return nil, false
		}
	}
	copy := *record
	return &copy, true
}

// Incoming context methods are never inspected until exact live caller identity
// has been proved against the whole admission. The finite caller implementation
// and its bounded cancellation source exist only in the reviewed test graph.
func (r *decisionAuditRouter) EmitDecisionAudit(ctx context.Context, record *store.DecisionAuditRecord) {
	// Ordinary production always exits here with empty admission and no clock,
	// settings proof, timer, NEW target or callback invoked.
	r.gate.Lock()
	candidate := r.admission != nil && !r.refreshPoison && !r.propagationLost && !r.state.closed && !r.state.fault && r.state.active < decisionAuditNewCapacity && r.state.observation.successful && r.sourceMatchesLocked(r.source) &&
		sameDecisionAuditReference(ctx, r.admission.manifest.binding.caller)
	source := r.source
	r.gate.Unlock()
	if !candidate || source == nil {
		r.legacy.EmitDecisionAudit(ctx, record)
		return
	}
	frozen, bounded := finiteDecisionAuditRecord(record)
	snapshot, observed := source.decisionAuditSnapshot() // outside the handoff gate
	r.gate.Lock()
	current := r.state.observation
	reading, clockOK := r.clockLocked()
	eligible := bounded && !r.refreshPoison && !r.propagationLost && !r.state.closed && !r.state.fault && r.state.active < decisionAuditNewCapacity && r.mutations == 0 &&
		r.sourceMatchesLocked(source) && sameDecisionAuditReference(ctx, r.admission.manifest.binding.caller) && ctx.Err() == nil &&
		clockOK && current.successful && observed.successful &&
		current.source == source && current.attachment == r.attachment && observed.attachment == current.attachment &&
		current.sequence == r.sequence && observed.sequence == current.sequence && current.generation == r.admission.manifest.generation &&
		current.epoch == reading.epoch && reading.tick >= current.q0 && reading.tick < current.deadline &&
		sameDecisionAuditSnapshot(snapshot, current.snapshot) && sameDecisionAuditSnapshot(observed.snapshot, current.snapshot) &&
		current.snapshot.Overrides[experiments.AuthorizationDecisionAuditV2] && r.server.experimentEnabledIn(current.snapshot, experiments.AuthorizationDecisionAuditV2)
	if !eligible {
		r.gate.Unlock()
		r.legacy.EmitDecisionAudit(ctx, record)
		return
	}
	// This locally owned cancellation context has no arbitrary parent watcher or
	// goroutine. Exact admitted caller cancellation is checked before/after dispatch;
	// the sole admitted timer provides the independent h+1s cancellation bound.
	localCtx, cancel := context.WithCancel(context.Background())
	slot := &decisionAuditNewSlot{cancel: cancel, done: make(chan struct{}), handoff: reading.tick, admission: r.admission, handler: r.contract.handler, clock: r.contract.clock, record: frozen, caller: ctx}
	r.slot = slot
	r.state.active = decisionAuditNewCapacity // exclusive owner linearization
	r.gate.Unlock()
	r.callNew(localCtx, slot)
}

func (r *decisionAuditRouter) callNew(ctx context.Context, slot *decisionAuditNewSlot) {
	failed := true
	var stop func()
	defer func() {
		if recover() != nil {
			failed = true
		}
		if stop != nil {
			func() {
				defer func() {
					if recover() != nil {
						failed = true
					}
				}()
				stop()
			}()
		}
		reading := readDecisionAuditClock(slot.clock)
		if !reading.valid || reading.epoch != slot.admission.manifest.clockEpoch || reading.tick < slot.handoff || reading.tick-slot.handoff > decisionAuditCompleteBudget {
			failed = true
		}
		slot.cancel()
		r.gate.Lock()
		if failed {
			r.state.failures++
			r.state.fault = true
			r.state.observation = decisionAuditRefreshObservation{}
			r.nextSequenceLocked()
		}
		r.state.active = 0
		r.slot = nil
		slot.caller = nil
		slot.record = nil
		slot.admission = nil
		slot.handler = nil
		slot.clock = nil
		slot.cancel = nil
		close(slot.done) // complete accounting/reference release before drain returns
		r.gate.Unlock()
	}()
	stop = slot.clock.CancelAt(slot.handoff+decisionAuditCancelBudget, slot.cancel)
	if stop == nil || ctx.Err() != nil {
		return
	}
	accepted, err := slot.handler.AcceptDecision(ctx, slot.record)
	failed = err != nil || !accepted.accepted || ctx.Err() != nil || slot.caller.Err() != nil
}

func (r *decisionAuditRouter) CloseNew(_ context.Context) error {
	r.gate.Lock()
	r.state.closed = true
	r.state.observation = decisionAuditRefreshObservation{}
	r.nextSequenceLocked()
	var cancel context.CancelFunc
	var done <-chan struct{}
	if r.slot != nil {
		cancel = r.slot.cancel
		done = r.slot.done
	}
	r.gate.Unlock()
	if cancel != nil {
		cancel()
		// Only a positively reviewed complete-return bound can create this slot.
		// Wait for its original h-based completion; no new timeout/grace or abandon.
		<-done
	}
	return nil
}

func (r *decisionAuditRouter) inspect() decisionAuditRouterState {
	r.gate.Lock()
	defer r.gate.Unlock()
	state := r.state
	state.observation.snapshot = cloneDecisionAuditSnapshot(state.observation.snapshot)
	return state
}

func (r *decisionAuditRouter) healthProjection() (string, string) {
	r.gate.Lock()
	fault := r.state.fault
	r.gate.Unlock()
	newHealth := "healthy"
	if fault {
		newHealth = decisionAuditFaultWarning
	}
	// Critical audit severity is independent of Hub-availability criticality.
	// Read historical legacy state only after releasing the router gate.
	if r.server == nil || r.server.decisionAuditWriter == nil {
		return newHealth, "healthy"
	}
	writer := r.server.decisionAuditWriter
	writer.mu.Lock()
	closed := writer.closed
	writer.mu.Unlock()
	if closed {
		return newHealth, "unhealthy: legacy decision audit writer closed"
	}
	for _, reason := range []DecisionAuditDropReason{DecisionAuditDropQueueFull, DecisionAuditDropWriteFailed, DecisionAuditDropShutdown} {
		for _, decision := range []string{"allow", "deny", "unknown"} {
			if writer.droppedCount(reason, decision) > 0 {
				return newHealth, "unhealthy: historical legacy decision audit drops"
			}
		}
	}
	return newHealth, "healthy"
}
