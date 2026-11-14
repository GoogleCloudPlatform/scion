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

package control

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/tmuxdeliver"
	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	core "github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// RouteDeliverMessage is the deliver-message route (design v2.22,
// .design/conduit/v2.22-deliver-message.md): one message flush into the
// harness's tmux session, from a fixed schema. It is registered only when
// Options.Deliver is set, which sciontool does only for an agent launched
// with hub.conduit_deliver and with tmux on PATH (DeliverEnabled).
const RouteDeliverMessage = agentcontrol.RouteDeliverMessage

// Key store bounds (design v2.22 §5.5 layer 2).
const (
	deliverStoreMax = 1024
	deliverStoreTTL = 15 * time.Minute
)

// targetCheckArgv checks that the harness's tmux window exists before any
// step runs. A constant: nothing in it comes from a request.
var targetCheckArgv = []string{"tmux", "has-session", "-t", tmuxdeliver.Target}

// DeliverOptions configure the deliver-message route.
type DeliverOptions struct {
	// Runner runs tmux (and the readiness probe) in the agent container,
	// as the user that owns the harness's tmux server. Required.
	Runner tmuxdeliver.Runner
	// Now and Sleep replace the clock in tests; nil uses the real clock.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) bool
	// ReadyPollInterval overrides the readiness poll interval (tests).
	ReadyPollInterval time.Duration
}

// deliverRoute serves deliver-message. One flush runs at a time; a second
// request gets 409 busy (another key) or in_progress (the same key).
type deliverRoute struct {
	runner   tmuxdeliver.Runner
	now      func() time.Time
	sleep    func(ctx context.Context, d time.Duration) bool
	poll     time.Duration
	memo     tmuxdeliver.MemoStore
	bufSeq   atomic.Uint64
	mu       sync.Mutex
	running  string // key of the running flush, "" when idle
	finished *deliverStore
}

func newDeliverRoute(o DeliverOptions) *deliverRoute {
	if o.Runner == nil {
		panic("control: DeliverOptions.Runner is required")
	}
	d := &deliverRoute{runner: o.Runner, now: o.Now, sleep: o.Sleep, poll: o.ReadyPollInterval}
	if d.now == nil {
		d.now = time.Now
	}
	d.finished = newDeliverStore(deliverStoreMax, deliverStoreTTL, d.now)
	return d
}

// deliverResult is one flush's answer and whether the key store keeps it.
type deliverResult struct {
	status int
	resp   agentcontrol.DeliverResponse
	store  bool
}

func (d *deliverRoute) serve(w http.ResponseWriter, r *http.Request) {
	receipt := d.now()
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeDeliver(w, invalid(agentcontrol.FieldBody))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, core.MaxRPCBody+1))
	if err != nil || len(body) > core.MaxRPCBody {
		writeDeliver(w, invalid(agentcontrol.FieldBody))
		return
	}
	req, err := agentcontrol.DecodeDeliverRequest(body)
	if err == nil {
		err = req.Validate()
	}
	if err != nil {
		var se *agentcontrol.SizeError
		if errors.As(err, &se) {
			log.Info("Control: deliver-message refused as too large (field %s)", se.Field)
			writeDeliver(w, deliverResult{status: http.StatusRequestEntityTooLarge, resp: agentcontrol.DeliverResponse{Outcome: agentcontrol.OutcomeTooLarge, Field: se.Field}})
			return
		}
		var fe *agentcontrol.FieldError
		field := agentcontrol.FieldBody
		if errors.As(err, &fe) {
			field = fe.Field
		}
		log.Info("Control: deliver-message refused (field %s)", field)
		writeDeliver(w, invalid(field))
		return
	}
	key := req.BrokerID + "\x00" + req.DeliveryID

	d.mu.Lock()
	if res, ok := d.finished.get(key); ok {
		d.mu.Unlock()
		res.resp.Replayed = true
		d.logOutcome(req, res)
		writeDeliver(w, res)
		return
	}
	if d.running != "" {
		outcome := agentcontrol.OutcomeBusy
		if d.running == key {
			outcome = agentcontrol.OutcomeInProgress
		}
		d.mu.Unlock()
		res := deliverResult{status: http.StatusConflict, resp: agentcontrol.DeliverResponse{Outcome: outcome}}
		d.logOutcome(req, res)
		writeDeliver(w, res)
		return
	}
	d.running = key
	d.mu.Unlock()

	res := d.flush(r.Context(), req, receipt)

	d.mu.Lock()
	if res.store {
		d.finished.put(key, res)
	}
	d.running = ""
	d.mu.Unlock()
	d.logOutcome(req, res)
	writeDeliver(w, res)
}

// hardStop returns the instant after which no visible step may start: the
// earlier of receipt + deadline_ms and not_after_unix_ms, unless not_after
// looks like clock skew (already past at receipt, or earlier than receipt +
// deadline − DeliverSkewTolerance), when it is ignored (design v2.22 §1.2,
// §2.2). skewed reports that it was ignored.
func hardStop(receipt time.Time, deadlineMS, notAfterMS int64) (stop time.Time, skewed bool) {
	stop = receipt.Add(time.Duration(deadlineMS) * time.Millisecond)
	notAfter := time.UnixMilli(notAfterMS)
	if !notAfter.After(receipt) || notAfter.Before(stop.Add(-agentcontrol.DeliverSkewTolerance)) {
		return stop, true
	}
	if notAfter.Before(stop) {
		stop = notAfter
	}
	return stop, false
}

// errExpired: the hard stop passed before the first visible step.
var errExpired = errors.New("deliver-message: hard stop passed before the first visible step")

func (d *deliverRoute) flush(ctx context.Context, req *agentcontrol.DeliverRequest, receipt time.Time) deliverResult {
	stop, skewed := hardStop(receipt, req.DeadlineMS, req.NotAfterUnixMS)
	if skewed {
		log.Info("Control: deliver-message not_after ignored as clock skew; using the receipt bound")
	}
	if _, err := d.runner.Run(ctx, targetCheckArgv, nil); err != nil {
		return notDelivered(agentcontrol.StageTarget)
	}

	op := tmuxdeliver.Op(req.Op)
	if req.Ready != nil && op == tmuxdeliver.OpPaste {
		// The wait never runs past the hard stop: its deadline is capped at
		// the time left, and the first visible step is refused after it.
		maxWait := min(tmuxdeliver.MaxReadyTimeout, stop.Sub(d.now()))
		if maxWait <= 0 {
			return expired()
		}
		wr := tmuxdeliver.Wait(ctx, tmuxdeliver.WaitConfig{
			Runner:      d.runner,
			Timings:     readyTimings(req.Ready, d.poll),
			Memo:        &d.memo,
			Key:         "local",
			MaxDeadline: maxWait,
			MaxSettle:   tmuxdeliver.MaxReadySettle,
			Now:         d.now,
			Sleep:       d.sleep,
		})
		if wr.Outcome == tmuxdeliver.WaitCancelled {
			// The hub cancelled the call (RpcCancel); nothing was shown.
			return notDelivered(agentcontrol.StageTarget)
		}
	}

	treq := tmuxdeliver.Request{
		Op:            op,
		InterruptKeys: req.InterruptKeys,
		SubmitKeys:    req.SubmitKeys,
	}
	if req.Text != nil {
		treq.Text = *req.Text
		name, err := tmuxdeliver.BufferName(req.DeliveryID, d.bufSeq.Add(1))
		if err != nil {
			return invalid(agentcontrol.FieldDeliveryID)
		}
		treq.BufferName = name
	}
	if req.ExtraSubmits != nil {
		treq.ExtraSubmits = req.ExtraSubmits.Count
		treq.ExtraSubmitDelay = time.Duration(req.ExtraSubmits.DelayMS) * time.Millisecond
	}
	if err := tmuxdeliver.CheckRoute(treq); err != nil {
		var be *tmuxdeliver.BoundsError
		if errors.As(err, &be) {
			return invalid(be.Field)
		}
		return invalid(agentcontrol.FieldBody)
	}

	// Nothing, not even load-buffer, starts once the hard stop has passed.
	if !d.now().Before(stop) {
		return expired()
	}
	tr := &trackingRunner{inner: d.runner, now: d.now, stop: stop, interrupts: len(req.InterruptKeys)}
	err := tmuxdeliver.Deliver(ctx, tr, treq)
	return classify(err, tr)
}

// classify maps Deliver's result to the route's answer.
func classify(err error, tr *trackingRunner) deliverResult {
	var partial *tmuxdeliver.PartialError
	switch {
	case err == nil:
		return deliverResult{status: http.StatusOK, resp: agentcontrol.DeliverResponse{Outcome: agentcontrol.OutcomeDelivered}, store: true}
	case errors.Is(err, errExpired) && tr.visible:
		// The interrupt keys were sent, then the hard stop passed before
		// the paste: something visible happened, so the flush is partial
		// (stored, never retried), not a 408 the hub would unbind on.
		return partialResult(agentcontrol.StagePaste)
	case errors.Is(err, errExpired):
		return expired()
	case errors.As(err, &partial):
		stage := agentcontrol.StageSubmit
		if partial.Stage == tmuxdeliver.StageExtraSubmit {
			stage = agentcontrol.StageExtraSubmit
		}
		return partialResult(stage)
	case tr.visible:
		stage := tr.failStage
		if stage == "" {
			stage = agentcontrol.StageSubmit
		}
		return partialResult(stage)
	case errors.Is(err, tmuxdeliver.ErrEmptyAfterSanitize):
		return invalid(agentcontrol.FieldText)
	case tr.failStage == agentcontrol.StageLoadBuffer:
		return notDelivered(agentcontrol.StageLoadBuffer)
	case tr.ran == 0:
		// Deliver refused the request before running anything.
		return invalid(agentcontrol.FieldBody)
	default:
		return notDelivered(agentcontrol.StageLoadBuffer)
	}
}

// readyTimings maps the request's ready block to the wait's fallback
// timings, the way the broker maps a harness ready spec: the image's own
// ready spec (init-run line 2) replaces them when present.
func readyTimings(r *agentcontrol.ReadySpec, poll time.Duration) tmuxdeliver.Timings {
	t := tmuxdeliver.Timings{PollInterval: poll}
	settle := time.Duration(r.SettleMS) * time.Millisecond
	if r.Strategy == state.ReadyStrategyHook {
		t.Settle = settle
		t.Deadline = time.Duration(r.TimeoutMS) * time.Millisecond
	} else {
		t.SettleFallback = settle
	}
	return t
}

// trackingRunner wraps the route's Runner for one flush: it refuses the
// first visible step (a send-keys or paste-buffer) after the hard stop, and
// records which stage failed first.
type trackingRunner struct {
	inner      tmuxdeliver.Runner
	now        func() time.Time
	stop       time.Time
	interrupts int // interrupt keys the op sends before anything else

	ran       int
	keys      int
	visible   bool
	pasted    bool
	failStage string
}

func (t *trackingRunner) Run(ctx context.Context, argv []string, stdin io.Reader) (string, error) {
	verb := ""
	if len(argv) > 1 && argv[0] == "tmux" {
		verb = argv[1]
	}
	shows := verb == "send-keys" || verb == "paste-buffer"
	// No paste starts after the hard stop, even when interrupt keys were
	// already sent (review S-A-01); no first visible step starts after it
	// either. After an interrupt the flush is partial, never 408.
	if (verb == "paste-buffer" || (shows && !t.visible)) && !t.now().Before(t.stop) {
		if t.visible && t.failStage == "" {
			t.failStage = agentcontrol.StagePaste
		}
		return "", errExpired
	}
	stage := ""
	switch verb {
	case "send-keys":
		t.keys++
		stage = agentcontrol.StageSubmit
		if !t.pasted && t.keys <= t.interrupts {
			stage = agentcontrol.StageInterrupt
		}
	case "load-buffer":
		stage = agentcontrol.StageLoadBuffer
	case "paste-buffer":
		stage = agentcontrol.StagePaste
	}
	if verb != "delete-buffer" {
		t.ran++
	}
	if shows {
		t.visible = true
	}
	out, err := t.inner.Run(ctx, argv, stdin)
	if err != nil && t.failStage == "" && stage != "" {
		t.failStage = stage
	}
	if err == nil && verb == "paste-buffer" {
		t.pasted = true
	}
	return out, err
}

func invalid(field string) deliverResult {
	return deliverResult{status: http.StatusBadRequest, resp: agentcontrol.DeliverResponse{Outcome: agentcontrol.OutcomeInvalid, Field: field}}
}

func notDelivered(stage string) deliverResult {
	return deliverResult{status: http.StatusUnprocessableEntity, resp: agentcontrol.DeliverResponse{Outcome: agentcontrol.OutcomeNotDelivered, Stage: stage}}
}

// expired is stored: a replay of the same key, however late, must not paste
// what the first attempt reported as not pasted.
func expired() deliverResult {
	return deliverResult{status: http.StatusRequestTimeout, resp: agentcontrol.DeliverResponse{Outcome: agentcontrol.OutcomeExpiredBeforePaste}, store: true}
}

func partialResult(stage string) deliverResult {
	return deliverResult{status: http.StatusOK, resp: agentcontrol.DeliverResponse{Outcome: agentcontrol.OutcomePartial, Stage: stage}, store: true}
}

func writeDeliver(w http.ResponseWriter, res deliverResult) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(res.status)
	_ = json.NewEncoder(w).Encode(res.resp)
}

// logOutcome writes the content-free info line (design v2.22 §5.6): never
// the text or key names.
func (d *deliverRoute) logOutcome(req *agentcontrol.DeliverRequest, res deliverResult) {
	log.Info("%s", deliverLogLine(req, res))
}

// deliverLogLine is the info line for one answer: ids, sizes and the
// outcome only.
func deliverLogLine(req *agentcontrol.DeliverRequest, res deliverResult) string {
	size := 0
	if req.Text != nil {
		size = len(*req.Text)
	}
	return fmt.Sprintf("Control: deliver-message delivery_id=%s op=%s bytes=%d messages=%d status=%d outcome=%s stage=%s replayed=%t",
		req.DeliveryID, req.Op, size, len(req.MessageIDs), res.status, res.resp.Outcome, res.resp.Stage, res.resp.Replayed)
}

// deliverStore is the agent-side key store (design v2.22 §5.5 layer 2):
// finished outcomes by key, at most max entries, LRU, each kept for ttl.
// It holds outcomes only, never text.
type deliverStore struct {
	max   int
	ttl   time.Duration
	now   func() time.Time
	order *list.List // front = most recently used
	byKey map[string]*list.Element
}

type deliverEntry struct {
	key string
	at  time.Time
	res deliverResult
}

func newDeliverStore(max int, ttl time.Duration, now func() time.Time) *deliverStore {
	return &deliverStore{max: max, ttl: ttl, now: now, order: list.New(), byKey: map[string]*list.Element{}}
}

func (s *deliverStore) get(key string) (deliverResult, bool) {
	el, ok := s.byKey[key]
	if !ok {
		return deliverResult{}, false
	}
	e := el.Value.(*deliverEntry)
	if s.now().Sub(e.at) > s.ttl {
		s.order.Remove(el)
		delete(s.byKey, key)
		return deliverResult{}, false
	}
	s.order.MoveToFront(el)
	return e.res, true
}

func (s *deliverStore) put(key string, res deliverResult) {
	if el, ok := s.byKey[key]; ok {
		s.order.Remove(el)
	}
	s.byKey[key] = s.order.PushFront(&deliverEntry{key: key, at: s.now(), res: res})
	for s.order.Len() > s.max {
		last := s.order.Back()
		s.order.Remove(last)
		delete(s.byKey, last.Value.(*deliverEntry).key)
	}
}

func (s *deliverStore) len() int { return s.order.Len() }

// DeliverEnabled reports whether sciontool should register deliver-message:
// the agent was launched with ExperimentConduitDeliver in
// SCION_HUB_EXPERIMENTS (set by the hub at dispatch) and tmux is available
// (hasTmux). Turning the experiment on later does not reach a running
// agent: it registers the route only after a restart.
func DeliverEnabled(getenv func(string) string, hasTmux bool) bool {
	if !hasTmux {
		return false
	}
	for _, name := range strings.Split(getenv(EnvHubExperiments), ",") {
		if strings.TrimSpace(name) == ExperimentConduitDeliver {
			return true
		}
	}
	return false
}

// EnvHubExperiments and ExperimentConduitDeliver: the launch-time flag.
const (
	EnvHubExperiments        = "SCION_HUB_EXPERIMENTS"
	ExperimentConduitDeliver = "hub.conduit_deliver"
)
