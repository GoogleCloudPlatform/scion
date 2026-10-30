package hub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// QuotaService provides quota enforcement for resource creation.
// It uses advisory locks to ensure atomic check-and-reserve under concurrency
// across Hub replicas (Postgres), and an in-process per-scope mutex to ensure
// the same atomicity within a single process — including single-node/SQLite
// deployments, where the advisory lock is a documented no-op (see
// entadapter.CompositeStore.TryAdvisoryLockObject) because there is only ever
// one process. Without the in-process mutex, two goroutines in that one
// process could still interleave the count-then-create sequence below (e.g.
// two concurrent agent starts at cap-1) and both succeed, over-allocating the
// scope by one (ptone/scion#1963).
type QuotaService struct {
	store  store.Store
	logger *slog.Logger

	// limitOverride, if set, is consulted by Reserve before
	// ResolveEffectiveLimit, implementing the "most specific wins"
	// precedence for per-broker settings (ptone/scion#2061 P2, design.md
	// §5.2): wired in server.go to brokerSettingLimitOverride, which returns
	// ok=true when limitName == store.LimitMaxAgentsPerBroker,
	// scopeType == store.QuotaScopeBroker, and the broker has its own
	// settings.maxAgents. ok=false means "no override — fall through to the
	// entitlement engine", which is also what effectiveBrokerLimit
	// (broker_capacity.go) does on the read side, so Reserve and every read
	// path agree (AC-P2-10). nil (the zero value) disables the hook
	// entirely, which is what every QuotaService built before P2 gets.
	limitOverride func(ctx context.Context, limitName, scopeType, scopeID string) (value int64, ok bool, err error)

	scopeLocksMu sync.Mutex
	scopeLocks   map[string]*sync.Mutex

	// enforced reports whether a given limit should be rejected once it is
	// exhausted (design P1-D5). When nil, or when it returns true, the limit
	// is enforced — this is the fail-safe default. When it returns false for
	// a limit, Reserve keeps counting (reservations, release, reconcile and
	// backfill are unaffected) but no longer rejects the create.
	enforced func(limitName string) bool

	// scopeConflictMetrics counts reservation scope conflicts; nil until
	// Server.SetQuotaScopeConflictMetrics.
	scopeConflictMetrics atomic.Pointer[OTelQuotaScopeConflictMetrics]

	// capacityMetrics counts backstop re-asserts and releases on
	// deregistered Runtime Broker scopes; nil until
	// Server.SetQuotaCapacityMetrics.
	capacityMetrics atomic.Pointer[OTelQuotaCapacityMetrics]

	// brokerBackfillPending holds, per Runtime Broker ID, a
	// *brokerBackfillRetry for a broker whose registration backfill skipped an
	// agent on this replica (Server.BackfillBrokerOnRegistration); its heartbeats
	// retry it (Server.retryBrokerBackfill).
	brokerBackfillPending sync.Map
}

// Sites of a reservation scope conflict, the site attribute of
// scion.hub.quota.reservation_scope_conflict.
const (
	// scopeConflictSiteReserve: Reserve refused a request, from any caller
	// (409 at the HTTP boundary; also the observed-phase re-reserve and the
	// move worker).
	scopeConflictSiteReserve = "reserve"
	// scopeConflictSiteReassert: a restart's re-assert found the agent's
	// reservation on another broker and left it.
	scopeConflictSiteReassert = "reassert"
	// scopeConflictSiteBackfill: the reconcile backfill found the agent's
	// reservation on another broker and left it.
	scopeConflictSiteBackfill = "backfill"
	// scopeConflictSiteSweep: the reconcile release pass found a pinned
	// agent's reservation on another broker and left it.
	scopeConflictSiteSweep = "sweep"
)

// OTelQuotaScopeConflictMetrics counts reservation scope conflicts (an
// agent's active max_agents_per_broker reservation on a broker other than
// the one it is admitted on or assigned to) as
// scion.hub.quota.reservation_scope_conflict, with attribute site
// (reserve, reassert, backfill or sweep). The reconcile sites count a
// conflict that persists once per pass.
type OTelQuotaScopeConflictMetrics struct {
	total metric.Int64Counter
}

// NewOTelQuotaScopeConflictMetrics creates the counter.
func NewOTelQuotaScopeConflictMetrics(mp metric.MeterProvider) (*OTelQuotaScopeConflictMetrics, error) {
	if mp == nil {
		return nil, errors.New("otel quota scope conflict metrics: nil MeterProvider")
	}
	c, err := mp.Meter(instrumentationScope).Int64Counter("scion.hub.quota.reservation_scope_conflict",
		metric.WithUnit("{conflict}"))
	if err != nil {
		return nil, err
	}
	return &OTelQuotaScopeConflictMetrics{total: c}, nil
}

// RecordScopeConflict counts one conflict at site.
func (m *OTelQuotaScopeConflictMetrics) RecordScopeConflict(site string) {
	m.total.Add(context.Background(), 1, metric.WithAttributes(attribute.String("site", site)))
}

// SetQuotaScopeConflictMetrics wires the
// scion.hub.quota.reservation_scope_conflict counter. Until it is set,
// conflicts are logged but not counted.
func (s *Server) SetQuotaScopeConflictMetrics(m *OTelQuotaScopeConflictMetrics) {
	if s.quotaService != nil {
		s.quotaService.scopeConflictMetrics.Store(m)
	}
}

// recordScopeConflict counts one reservation scope conflict at site.
func (qs *QuotaService) recordScopeConflict(site string) {
	if m := qs.scopeConflictMetrics.Load(); m != nil {
		m.RecordScopeConflict(site)
	}
}

// isEnforced reports whether limitName should be rejected once exhausted.
// A nil enforced func means every limit is enforced (fail-safe default).
func (qs *QuotaService) isEnforced(limitName string) bool {
	if qs.enforced == nil {
		return true
	}
	return qs.enforced(limitName)
}

// lockForScope returns the process-local mutex for (limitName, scopeType,
// scopeID), creating it on first use. Locks are never removed — the key
// space is bounded by the number of distinct (limit, scope) pairs actually
// exercised, which is small and stable (one entry per project/broker/etc
// that has ever hit CheckAndReserve).
func (qs *QuotaService) lockForScope(limitName, scopeType, scopeID string) *sync.Mutex {
	key := limitName + "\x00" + scopeType + "\x00" + scopeID
	qs.scopeLocksMu.Lock()
	defer qs.scopeLocksMu.Unlock()
	if qs.scopeLocks == nil {
		qs.scopeLocks = make(map[string]*sync.Mutex)
	}
	m, ok := qs.scopeLocks[key]
	if !ok {
		m = &sync.Mutex{}
		qs.scopeLocks[key] = m
	}
	return m
}

// ErrReservationScopeConflict is the sentinel a ReservationScopeConflictError
// unwraps to.
var ErrReservationScopeConflict = errors.New("quota: resource holds an active reservation in another scope")

// ErrCodeBrokerReservationScopeConflict is the API error code of a request
// refused with a ReservationScopeConflictError (HTTP 409). It is defined
// here with the error it names rather than in pkg/hub/apierr, as other
// feature-local codes are (access_constraint.go).
const ErrCodeBrokerReservationScopeConflict = "broker_reservation_scope_conflict"

// ReservationScopeConflictError is returned by Reserve for a broker-scoped
// request when the resource already holds an active reservation for the
// limit on another broker. The held reservation is kept, no reservation is
// made in the requested scope, and nothing is moved or released.
type ReservationScopeConflictError struct {
	Limit            string
	ResourceID       string
	HeldScopeID      string
	RequestedScopeID string
}

func (e *ReservationScopeConflictError) Error() string {
	return fmt.Sprintf("quota: %s: resource %s holds an active reservation on %s; it cannot be reserved on %s",
		e.Limit, e.ResourceID, e.HeldScopeID, e.RequestedScopeID)
}

func (e *ReservationScopeConflictError) Unwrap() error { return ErrReservationScopeConflict }

// QuotaExceededError is the store.ErrQuotaExceeded refusal of Reserve,
// naming the exhausted scope. It unwraps to store.ErrQuotaExceeded, so
// errors.Is callers are unchanged, and its message is that sentinel's.
type QuotaExceededError struct {
	Limit     string
	ScopeType string
	ScopeID   string
}

func (e *QuotaExceededError) Error() string { return store.ErrQuotaExceeded.Error() }

func (e *QuotaExceededError) Unwrap() error { return store.ErrQuotaExceeded }

// quotaExceededDetails returns the API error details of err when it is a
// broker-scoped QuotaExceededError: {runtimeBrokerId}. The broker's name is
// omitted, so the error path makes no broker read. nil otherwise.
func quotaExceededDetails(err error) map[string]interface{} {
	var exceeded *QuotaExceededError
	if !errors.As(err, &exceeded) || exceeded.ScopeType != store.QuotaScopeBroker || exceeded.ScopeID == "" {
		return nil
	}
	return map[string]interface{}{"runtimeBrokerId": exceeded.ScopeID}
}

// ErrQuotaLockContention is returned when the advisory lock cannot be acquired,
// indicating another concurrent request is checking the same quota scope.
// Callers may retry.
var ErrQuotaLockContention = errors.New("quota lock contention: retry")

// CheckAndReserve atomically checks quota and creates a reservation.
// It returns nil if the reservation succeeds, is already held, or if no
// limit is defined. It returns store.ErrQuotaExceeded if the quota is
// exhausted. It returns ErrQuotaLockContention if the advisory lock is held
// by another request.
func (qs *QuotaService) CheckAndReserve(ctx context.Context, limitName string, subjectID string, scopeType string, scopeID string, resourceID string) error {
	_, err := qs.Reserve(ctx, limitName, subjectID, scopeType, scopeID, resourceID)
	return err
}

// Reserve is CheckAndReserve that also reports whether this call created a
// new reservation (ptone/scion#1978). created is false when no limit is
// defined, the limit is unlimited, the resource already held an active
// reservation for this limit, or err is non-nil. For a broker-scoped
// request, an active reservation on another broker is refused with a
// *ReservationScopeConflictError whatever the cap or enforcement setting.
// Callers that roll back a reservation after a failed operation must only do
// so when created is true; otherwise they would release a reservation an
// earlier call made.
func (qs *QuotaService) Reserve(ctx context.Context, limitName string, subjectID string, scopeType string, scopeID string, resourceID string) (created bool, err error) {
	// 1. Look up LimitDefinition by name.
	limitDef, err := qs.store.GetLimitDefinitionByName(ctx, limitName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// No limit defined — no enforcement.
			return false, nil
		}
		return false, fmt.Errorf("quota: lookup limit definition %q: %w", limitName, err)
	}
	if limitDef == nil {
		// No limit defined — no enforcement.
		return false, nil
	}

	// 1b. Placement invariant (broker scope): an active reservation in
	// another broker's scope never satisfies this request, and one in this
	// scope is an idempotent retry. Checked before the unlimited short-circuit
	// and independent of enforcement, so neither an unlimited cap nor
	// enforcement being off turns a mismatched reservation into a success.
	if scopeType == store.QuotaScopeBroker {
		held, err := qs.store.GetActiveReservation(ctx, limitDef.ID, resourceID)
		switch {
		case err == nil:
			return false, qs.classifyHeldReservation(limitName, held, scopeType, scopeID)
		case !errors.Is(err, store.ErrNotFound):
			return false, fmt.Errorf("quota: check existing reservation for %q: %w", limitName, err)
		}
	}

	// 2. Resolve effective limit for the subject, consulting limitOverride
	// first (design.md §5.2 precedence).
	effectiveLimit, err := qs.effectiveLimitForReserve(ctx, limitName, limitDef.ID, subjectID, scopeType, scopeID)
	if err != nil {
		return false, fmt.Errorf("quota: resolve effective limit for %q: %w", limitName, err)
	}

	// 3. Unlimited — no enforcement.
	if effectiveLimit <= 0 {
		return false, nil
	}

	// 4. Acquire the in-process scope lock first (see lockForScope): this is
	// what actually serializes concurrent goroutines within this Hub process,
	// including on SQLite where the advisory lock below is a no-op.
	scopeLock := qs.lockForScope(limitName, scopeType, scopeID)
	scopeLock.Lock()
	defer scopeLock.Unlock()

	// 4b. Acquire advisory lock scoped to (quota class, scope hash). This is
	// the cross-replica primitive (Postgres); on SQLite it always succeeds
	// without providing mutual exclusion, which is why 4 above exists.
	locker, ok := qs.store.(store.AdvisoryLocker)
	if !ok {
		return false, fmt.Errorf("quota enforcement unavailable: store does not support advisory locks")
	}

	objID := store.StableProjectHash(scopeID)
	acquired, release, err := locker.TryAdvisoryLockObject(ctx, store.LockQuotaEnforcement, objID)
	if err != nil {
		return false, fmt.Errorf("quota: advisory lock for %q: %w", limitName, err)
	}
	defer func() {
		if releaseErr := release(); releaseErr != nil {
			qs.logger.Warn("failed to release quota advisory lock",
				"limit", limitName, "error", releaseErr)
		}
	}()

	if !acquired {
		if !qs.isEnforced(limitName) {
			// Enforcement is off: let the request proceed without a
			// reservation rather than 429 on lock contention. The reconcile
			// backfill picks up the resource within its next pass
			// (design P1-D5).
			qs.logger.Debug("quota lock contention while enforcement is off, proceeding without reservation",
				"limit", limitName, "scope_type", scopeType, "scope_id", scopeID)
			return false, nil
		}
		return false, ErrQuotaLockContention
	}

	// 4.5. Idempotency (#1963): a resource that already holds an active
	// reservation for this limit must not accumulate a second one. Without
	// this, re-reserving on every agent start/resume (necessary because stop
	// and suspend now release the reservation) would let repeated
	// start/stop cycles — or a start call against an agent that is already
	// counted (e.g. a fresh create, or start called again on an already-
	// running agent) — silently consume extra quota slots for one resource.
	// Checked under the advisory lock so a concurrent release can't race this
	// read. A broker-scoped row is classified as in step 1b: another goroutine
	// of this process may have reserved it since.
	held, err := qs.store.GetActiveReservation(ctx, limitDef.ID, resourceID)
	switch {
	case err == nil:
		if scopeType == store.QuotaScopeBroker {
			return false, qs.classifyHeldReservation(limitName, held, scopeType, scopeID)
		}
		return false, nil
	case !errors.Is(err, store.ErrNotFound):
		return false, fmt.Errorf("quota: check existing reservation for %q: %w", limitName, err)
	}

	// 5. Count active reservations.
	count, err := qs.store.CountActiveReservations(ctx, limitDef.ID, subjectID, scopeType, scopeID)
	if err != nil {
		return false, fmt.Errorf("quota: count active reservations for %q: %w", limitName, err)
	}

	// 6. Check quota. When enforcement is off for this limit, the cap is
	// still tracked (the reservation below is created) but the create is not
	// rejected (design P1-D5): counts stay accurate for when the switch is
	// flipped back on, and the usage page keeps showing the true count
	// (e.g. "31 / 16") while it is off.
	if count >= effectiveLimit {
		if !qs.isEnforced(limitName) {
			qs.logger.Debug("quota exceeded but enforcement is off, creating reservation anyway",
				"limit", limitName, "scope_type", scopeType, "scope_id", scopeID,
				"count", count, "effective_limit", effectiveLimit)
		} else {
			return false, &QuotaExceededError{Limit: limitName, ScopeType: scopeType, ScopeID: scopeID}
		}
	}

	// 7. Create reservation.
	reservation := &store.UsageReservation{
		LimitDefinitionID: limitDef.ID,
		SubjectID:         subjectID,
		ScopeType:         scopeType,
		ScopeID:           scopeID,
		ResourceID:        resourceID,
		Reserved:          1,
	}
	if scopeType == store.QuotaScopeBroker {
		return qs.insertBrokerReservation(ctx, limitName, reservation)
	}
	if _, err := qs.store.CreateUsageReservation(ctx, reservation); err != nil {
		return false, fmt.Errorf("quota: create reservation for %q: %w", limitName, err)
	}

	return true, nil
}

// TransferOpts are the TransferBrokerReservation request flags a caller of
// QuotaService.TransferBroker or transferBrokerReservationUnchecked sets
// (see store.BrokerReservationTransfer).
type TransferOpts struct {
	CreateIfAbsent    bool
	RequireAgentOnTo  bool
	FromReservationID string
	Cond              *store.TransferAgentCond
}

// TransferBroker moves agentID's max_agents_per_broker reservation onto
// toBroker atomically (store.TransferBrokerReservation), with the cap check
// on toBroker. The source slot is freed only in the commit that secures the
// target, so the source needs no lock. limited is false (and nothing is
// written) when no limit definition exists.
//
// It joins Reserve's locking protocol on the target scope: on an unlimited
// target it takes no lock and moves the row unchecked (Reserve writes no row
// there, but the agent's existing row must not be lost); otherwise it takes
// the in-process scope mutex, then the advisory lock, and counts inside the
// transaction while holding both. Lock contention returns
// ErrQuotaLockContention when enforcement is on, and proceeds unchecked when
// it is off; with enforcement off the cap is not applied. Both unlocks are
// deferred, so they run after the transaction has committed or rolled back.
// A full target returns a *QuotaExceededError naming it, and the source
// reservation is unchanged.
func (qs *QuotaService) TransferBroker(ctx context.Context, agentID, toBroker string, o TransferOpts) (res store.BrokerReservationTransferResult, limited bool, err error) {
	const limitName = store.LimitMaxAgentsPerBroker
	req, limited, err := brokerTransferRequest(ctx, qs.store, agentID, toBroker, o)
	if err != nil || !limited {
		return res, false, err
	}

	effectiveLimit, err := qs.effectiveLimitForReserve(ctx, limitName, req.LimitDefinitionID, toBroker, store.QuotaScopeBroker, toBroker)
	if err != nil {
		return res, true, fmt.Errorf("quota: resolve effective limit for %q: %w", limitName, err)
	}
	if effectiveLimit > 0 {
		scopeLock := qs.lockForScope(limitName, store.QuotaScopeBroker, toBroker)
		scopeLock.Lock()
		defer scopeLock.Unlock()

		locker, ok := qs.store.(store.AdvisoryLocker)
		if !ok {
			return res, true, fmt.Errorf("quota enforcement unavailable: store does not support advisory locks")
		}
		acquired, release, err := locker.TryAdvisoryLockObject(ctx, store.LockQuotaEnforcement, store.StableProjectHash(toBroker))
		if err != nil {
			return res, true, fmt.Errorf("quota: advisory lock for %q: %w", limitName, err)
		}
		defer func() {
			if releaseErr := release(); releaseErr != nil {
				qs.logger.Warn("failed to release quota advisory lock",
					"limit", limitName, "error", releaseErr)
			}
		}()
		enforced := qs.isEnforced(limitName)
		if !acquired && enforced {
			return res, true, ErrQuotaLockContention
		}
		if enforced {
			req.Cap = effectiveLimit
		}
	}

	res, err = qs.store.TransferBrokerReservation(ctx, req)
	if errors.Is(err, store.ErrQuotaExceeded) {
		return res, true, &QuotaExceededError{Limit: limitName, ScopeType: store.QuotaScopeBroker, ScopeID: toBroker}
	}
	if err != nil {
		return res, true, fmt.Errorf("quota: transfer reservation for %q: %w", limitName, err)
	}
	return res, true, nil
}

// brokerTransferRequest looks up the max_agents_per_broker limit definition
// in st and builds the TransferBrokerReservation request moving agentID's
// reservation onto toBroker with o's flags (Cap 0). limited is false, with
// no error, when no limit definition exists. An empty toBroker is refused
// with store.ErrInvalidTransfer before any lookup: no reservation may land
// on an empty broker scope.
func brokerTransferRequest(ctx context.Context, st store.Store, agentID, toBroker string, o TransferOpts) (req store.BrokerReservationTransfer, limited bool, err error) {
	const limitName = store.LimitMaxAgentsPerBroker
	if toBroker == "" {
		return req, false, fmt.Errorf("quota: transfer reservation for %q to an empty broker ID: %w", limitName, store.ErrInvalidTransfer)
	}
	def, err := st.GetLimitDefinitionByName(ctx, limitName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return req, false, nil
		}
		return req, false, fmt.Errorf("quota: lookup limit definition %q: %w", limitName, err)
	}
	if def == nil {
		return req, false, nil
	}
	return store.BrokerReservationTransfer{
		LimitDefinitionID: def.ID,
		AgentID:           agentID,
		ToBrokerID:        toBroker,
		CreateIfAbsent:    o.CreateIfAbsent,
		RequireAgentOnTo:  o.RequireAgentOnTo,
		FromReservationID: o.FromReservationID,
		Cond:              o.Cond,
	}, true, nil
}

// maxBrokerReservationInserts bounds insertBrokerReservation: the first
// insert, plus one retry when the racing winner was released before it could
// be read back.
const maxBrokerReservationInserts = 2

// insertBrokerReservation inserts a broker-scoped reservation. The unique
// active (limit_definition_id, resource_id) index is the serializer: when a
// racing insert won (another Hub replica, or a request for another broker,
// whose scope locks differ from this one's), the winner is read back and
// classified as in Reserve's step 1b, a retry in the same scope
// (created=false) or a ReservationScopeConflictError. When the winner was
// released before the read, the insert is retried once; losing that race
// again returns ErrQuotaLockContention.
func (qs *QuotaService) insertBrokerReservation(ctx context.Context, limitName string, reservation *store.UsageReservation) (bool, error) {
	for attempt := 0; attempt < maxBrokerReservationInserts; attempt++ {
		_, err := qs.store.CreateUsageReservation(ctx, reservation)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, store.ErrActiveReservationExists) {
			return false, fmt.Errorf("quota: create reservation for %q: %w", limitName, err)
		}
		held, err := qs.store.GetActiveReservation(ctx, reservation.LimitDefinitionID, reservation.ResourceID)
		if err == nil {
			return false, qs.classifyHeldReservation(limitName, held, reservation.ScopeType, reservation.ScopeID)
		}
		if !errors.Is(err, store.ErrNotFound) {
			return false, fmt.Errorf("quota: read racing reservation for %q: %w", limitName, err)
		}
	}
	return false, ErrQuotaLockContention
}

// classifyHeldReservation classifies a resource's active reservation against
// a request in (scopeType, scopeID): nil when it is in that scope (an
// idempotent retry, which creates nothing), a *ReservationScopeConflictError
// otherwise. A conflict is counted at site "reserve".
func (qs *QuotaService) classifyHeldReservation(limitName string, held *store.UsageReservation, scopeType, scopeID string) error {
	if held.ScopeType == scopeType && held.ScopeID == scopeID {
		return nil
	}
	qs.recordScopeConflict(scopeConflictSiteReserve)
	return &ReservationScopeConflictError{
		Limit:            limitName,
		ResourceID:       held.ResourceID,
		HeldScopeID:      held.ScopeID,
		RequestedScopeID: scopeID,
	}
}

// effectiveLimitForReserve resolves the limit Reserve should enforce for
// (limitName, scopeType, scopeID): limitOverride, if set, gets first say;
// ok=false from it (or a nil hook) falls through to ResolveEffectiveLimit.
// This is the enforcement-side half of the P2-D2 precedence (design.md
// §5.2); effectiveBrokerLimit (broker_capacity.go) is the read-side half,
// consulting the same override hook so both sides agree (AC-P2-10).
func (qs *QuotaService) effectiveLimitForReserve(ctx context.Context, limitName, limitDefID, subjectID, scopeType, scopeID string) (int64, error) {
	if qs.limitOverride != nil {
		value, ok, err := qs.limitOverride(ctx, limitName, scopeType, scopeID)
		if err != nil {
			return 0, fmt.Errorf("limit override: %w", err)
		}
		if ok {
			return value, nil
		}
	}
	return qs.ResolveEffectiveLimit(ctx, limitDefID, subjectID, scopeType, scopeID)
}

// ResolveEffectiveLimit determines the effective limit using the merge rule:
// most generous (maximum value) wins across all matching bindings.
// Value <= 0 means "unlimited" — if ANY binding grants unlimited, the result
// is unlimited (0) because that is the most generous possible limit.
func (qs *QuotaService) ResolveEffectiveLimit(ctx context.Context, limitDefID string, subjectID string, scopeType string, scopeID string) (int64, error) {
	value, _, err := qs.resolveEffectiveLimitWithSource(ctx, limitDefID, subjectID, scopeType, scopeID)
	return value, err
}

// resolveEffectiveLimitWithSource is ResolveEffectiveLimit, plus whether the
// value came from a matching entitlement binding (fromBinding=true) rather
// than the limit definition's hub-wide default (fromBinding=false). Used by
// effectiveBrokerLimit (broker_capacity.go) to report a "source" alongside
// the value (design.md §5.9); ResolveEffectiveLimit itself, and every
// existing caller, is unchanged.
func (qs *QuotaService) resolveEffectiveLimitWithSource(ctx context.Context, limitDefID string, subjectID string, scopeType string, scopeID string) (value int64, fromBinding bool, err error) {
	// Collect all matching bindings from every source.
	var matchingBindings []*store.EntitlementBinding

	// 1. Collect user-specific bindings.
	userBindings, err := qs.store.ListEntitlementBindingsForSubject(ctx, store.EntitlementSubjectUser, subjectID)
	if err != nil {
		return 0, false, fmt.Errorf("list user bindings: %w", err)
	}
	for _, b := range userBindings {
		if b.LimitDefinitionID == limitDefID && matchesScope(b, scopeType, scopeID) {
			matchingBindings = append(matchingBindings, b)
		}
	}

	// 2. Collect group bindings via user's group memberships.
	groupMemberships, err := qs.store.GetUserGroups(ctx, subjectID)
	if err != nil {
		// Not all store implementations may have groups; log and continue.
		qs.logger.Debug("failed to get user groups for quota resolution",
			"subject_id", subjectID, "error", err)
	} else {
		for _, gm := range groupMemberships {
			groupBindings, err := qs.store.ListEntitlementBindingsForSubject(ctx, store.EntitlementSubjectGroup, gm.GroupID)
			if err != nil {
				qs.logger.Debug("failed to list group bindings",
					"group_id", gm.GroupID, "error", err)
				continue
			}
			for _, b := range groupBindings {
				if b.LimitDefinitionID == limitDefID && matchesScope(b, scopeType, scopeID) {
					matchingBindings = append(matchingBindings, b)
				}
			}
		}
	}

	// 3. Collect system default bindings.
	sysBindings, err := qs.store.ListEntitlementBindingsForSubject(ctx, store.EntitlementSubjectSystemDefault, "")
	if err != nil {
		qs.logger.Debug("failed to list system default bindings", "error", err)
	} else {
		for _, b := range sysBindings {
			if b.LimitDefinitionID == limitDefID && matchesScope(b, scopeType, scopeID) {
				matchingBindings = append(matchingBindings, b)
			}
		}
	}

	// 4. If no binding found, fall back to limit definition's default value.
	if len(matchingBindings) == 0 {
		limitDef, err := qs.store.GetLimitDefinition(ctx, limitDefID)
		if err != nil {
			return 0, false, fmt.Errorf("get limit definition: %w", err)
		}
		// DefaultValue <= 0 means unlimited (return 0).
		if limitDef.DefaultValue <= 0 {
			return 0, false, nil
		}
		return limitDef.DefaultValue, false, nil
	}

	// 5. Apply merge rule: if ANY binding grants unlimited (Value <= 0),
	// the result is unlimited — that's the most generous possible limit.
	for _, b := range matchingBindings {
		if b.Value <= 0 {
			return 0, true, nil // unlimited, but still an explicit entitlement grant
		}
	}

	// 6. Otherwise, take the maximum positive value (most generous finite limit).
	var maxValue int64
	for _, b := range matchingBindings {
		if b.Value > maxValue {
			maxValue = b.Value
		}
	}

	return maxValue, true, nil
}

// Release marks resourceID's active reservation for limitName as released
// (best-effort). It touches only that limit: a resource can hold
// reservations under several limits (an agent holds both
// max_agents_per_broker and max_agents_per_project), and releasing one must
// not release the others (ptone/scion#1978). It is a no-op when the limit is
// not defined or the resource holds no active reservation for it. Errors
// are logged but not returned: deletion must not be blocked by quota
// bookkeeping.
func (qs *QuotaService) Release(ctx context.Context, limitName string, resourceID string) {
	limitDef, err := qs.store.GetLimitDefinitionByName(ctx, limitName)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			qs.logger.Warn("failed to release quota reservation: limit lookup failed",
				"limit", limitName, "resource_id", resourceID, "error", err)
		}
		return
	}
	if limitDef == nil {
		// No limit defined — nothing to release.
		return
	}
	if err := qs.store.ReleaseReservation(ctx, limitDef.ID, resourceID); err != nil && !errors.Is(err, store.ErrNotFound) {
		qs.logger.Warn("failed to release quota reservation",
			"limit", limitName, "resource_id", resourceID, "error", err)
	}
}

// matchesScope checks whether a binding's scope matches the requested scope.
// A system-scoped binding matches any request scope (it applies globally).
func matchesScope(b *store.EntitlementBinding, scopeType, scopeID string) bool {
	// System-scoped bindings apply to all scopes.
	if b.ScopeType == store.QuotaScopeSystem {
		return true
	}
	return b.ScopeType == scopeType && b.ScopeID == scopeID
}
