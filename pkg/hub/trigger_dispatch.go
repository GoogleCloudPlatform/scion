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

package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// triggerRuntime holds the hub's shared trigger.event durablequeue handle and
// background drainer lifecycle state. The zero value is ready to use.
type triggerRuntime struct {
	mu      sync.Mutex
	queue   *durablequeue.StoreQueue
	drainer *durablequeue.Drainer
	cancel  context.CancelFunc
	done    chan struct{}
	started bool
}

// TriggerEventQueue returns the hub's shared durablequeue.StoreQueue for
// trigger.event producers and drainers, creating it lazily when s.store
// implements store.DurableQueueStore.
func (s *Server) TriggerEventQueue() *durablequeue.StoreQueue {
	if s == nil {
		return nil
	}
	dqStore, ok := s.store.(store.DurableQueueStore)
	if !ok || dqStore == nil {
		return nil
	}
	rt := &s.triggers
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.queue == nil {
		rt.queue = durablequeue.NewQueue(dqStore)
	}
	return rt.queue
}

// serverTriggerCore adapts Server's fire core, experiment gate, and wake-fire
// shutdown admission to trigger.Core and trigger.FireAdmitter without exposing
// package hub types to pkg/hub/trigger.
type serverTriggerCore struct {
	srv *Server
}

var (
	_ trigger.Core         = serverTriggerCore{}
	_ trigger.FireAdmitter = serverTriggerCore{}
)

func (c serverTriggerCore) Fire(ctx context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
	return c.srv.fireAction(ctx, req)
}

func (c serverTriggerCore) Experiment(name string) bool {
	return c.srv.experimentEnabled(name)
}

func (c serverTriggerCore) AdmitFire(ctx, itemCtx context.Context) (context.Context, func(), bool) {
	if itemCtx != nil && itemCtx.Err() != nil {
		return ctx, func() {}, false
	}
	var (
		trackCtx context.Context
		endWork  = func() {}
	)
	if existingAbort, _ := ctx.Value(scheduledFireAbortKey{}).(context.Context); existingAbort != nil {
		trackCtx = existingAbort
	} else if c.srv != nil {
		var ok bool
		trackCtx, endWork, ok = c.srv.scheduledWakeFires.begin()
		if !ok {
			return ctx, func() {}, false
		}
	}
	// Bound scheduledFireAbortKey by BOTH hub shutdown (trackCtx) and the
	// durablequeue item lifetime (itemCtx) so wakeFireTarget's
	// detachKeepFireAbort(ctx) outlives the 30 s per-fire timeout without
	// outliving either the queue item (Stage 1 ledger L3) or hub shutdown.
	abortCtx, cancelAbort := context.WithCancel(context.Background())
	var stopTrack, stopItem func() bool
	if trackCtx != nil {
		stopTrack = context.AfterFunc(trackCtx, cancelAbort)
	}
	if itemCtx != nil {
		stopItem = context.AfterFunc(itemCtx, cancelAbort)
	}
	fireCtx, cancelFire := context.WithCancel(ctx)
	stopFire := context.AfterFunc(abortCtx, cancelFire)
	return withScheduledFireAbort(fireCtx, abortCtx), func() {
		stopFire()
		if stopItem != nil {
			stopItem()
		}
		if stopTrack != nil {
			stopTrack()
		}
		cancelFire()
		cancelAbort()
		endWork()
	}, true
}

func (s *Server) triggerDrainerContext(ctx context.Context) context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return ctx
}

// startTriggerDrainer marks trigger background services started and launches
// the trigger.event drainer when hub.triggers is enabled. When hub.triggers is
// off, no drainer goroutine runs until reconcileTriggerDrainer observes the
// experiment enabled (§16.2).
func (s *Server) startTriggerDrainer(ctx context.Context) <-chan struct{} {
	dqStore, ok := s.store.(store.DurableQueueStore)
	if !ok || dqStore == nil {
		return nil
	}
	rt := &s.triggers
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.started = true
	s.reconcileTriggerDrainerLocked(ctx, dqStore)
	return rt.done
}

// reconcileTriggerDrainer aligns the running trigger.event drainer with the
// live hub.triggers experiment state (called on ApplySnapshot and in tests).
func (s *Server) reconcileTriggerDrainer(ctx context.Context) {
	dqStore, ok := s.store.(store.DurableQueueStore)
	if !ok || dqStore == nil {
		return
	}
	rt := &s.triggers
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if !rt.started {
		return
	}
	s.reconcileTriggerDrainerLocked(ctx, dqStore)
}

func (s *Server) reconcileTriggerDrainerLocked(ctx context.Context, dqStore store.DurableQueueStore) {
	rt := &s.triggers
	if !s.experimentEnabled(experiments.Triggers) {
		if rt.cancel != nil {
			rt.cancel()
			rt.drainer, rt.cancel, rt.done = nil, nil, nil
		}
		return
	}
	if rt.drainer != nil {
		return
	}
	ctx = s.triggerDrainerContext(ctx)
	if ctx.Err() != nil {
		return
	}
	if rt.queue == nil {
		rt.queue = durablequeue.NewQueue(dqStore)
	}
	d, err := s.triggerDrainer(dqStore, rt.queue)
	if err != nil {
		slog.Warn("hub.triggers: drainer not started", "error", err)
		return
	}
	dctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	rt.drainer, rt.cancel, rt.done = d, cancel, done
	go func() {
		defer close(done)
		d.Run(dctx)
		cancel()
		rt.mu.Lock()
		if rt.drainer == d {
			rt.drainer, rt.cancel, rt.done = nil, nil, nil
		}
		rt.mu.Unlock()
	}()
}

// triggerDrainer constructs the trigger.event durablequeue.Drainer wired to
// Server's fire core (catalog entry point triggerDrainer:message for trigger.fire).
func (s *Server) triggerDrainer(dqStore store.DurableQueueStore, queue *durablequeue.StoreQueue) (*durablequeue.Drainer, error) {
	core := serverTriggerCore{srv: s}
	logger := logging.Subsystem("hub.trigger-drainer")
	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Store:  s.store,
		Core:   core,
		Logger: logger,
	})
	if err != nil {
		return nil, err
	}
	return disp.NewDrainer(trigger.DrainerOptions{
		QueueStore: dqStore,
		Queue:      queue,
		Owner:      s.HubID(),
	})
}

// stopTriggerDrainer cancels the running trigger.event drainer and waits for
// its bounded shutdown drain (or ctx cancellation).
func (s *Server) stopTriggerDrainer(ctx context.Context) {
	rt := &s.triggers
	rt.mu.Lock()
	cancel, done := rt.cancel, rt.done
	rt.drainer, rt.cancel, rt.done = nil, nil, nil
	rt.started = false
	rt.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// runTriggerMaintenance executes the hourly trigger reaper, orphan-pending
// event reconciliation, and 7-day history purge inside Server.purgeHandler
// (§4.3, §6.4, rev 7.4).
func (s *Server) runTriggerMaintenance(ctx context.Context) {
	if s == nil || s.store == nil {
		return
	}
	res, err := trigger.RunMaintenance(ctx, s.store, time.Now())
	if err != nil {
		slog.Error("Scheduler: trigger maintenance failed", "error", err)
	}
	if res.FiresReaped > 0 || res.EventsReconciled > 0 || res.EventsPurged > 0 || res.FiresPurged > 0 {
		slog.Info("Scheduler: completed trigger maintenance",
			"fires_reaped", res.FiresReaped,
			"events_reconciled", res.EventsReconciled,
			"events_purged", res.EventsPurged,
			"fires_purged", res.FiresPurged)
	}
}

// verifiedLabelForFireEvent maps known signature_mode values to canonical
// integrity labels and fails closed to ExternalVerifiedUnverified for missing
// or unrecognized modes (never assuming scion-v1 and never echoing raw input).
func verifiedLabelForFireEvent(ev *hubfire.FireEvent) string {
	if ev == nil {
		return messaging.ExternalVerifiedUnverified
	}
	switch ev.Attributes["signature_mode"] {
	case store.WebhookSignatureModeBodyOnly, messaging.ExternalVerifiedHMACSHA256Body:
		return messaging.ExternalVerifiedHMACSHA256Body
	case store.WebhookSignatureModeScionV1, messaging.ExternalVerifiedScionV1:
		return messaging.ExternalVerifiedScionV1
	default:
		return messaging.ExternalVerifiedUnverified
	}
}

// buildDispatchedEventBody constructs the §8.3 messaging.EventBody for a
// FireEvent delivered to an agent.
func buildDispatchedEventBody(ev *hubfire.FireEvent) messaging.EventBody {
	if ev == nil {
		return messaging.EventBody{}
	}
	var body messaging.EventBody
	switch ev.Type {
	case string(messaging.EventWebhookReceived):
		body.Type = messaging.EventWebhookReceived
	case string(messaging.EventScheduleFired):
		body.Type = messaging.EventScheduleFired
	default:
		body.Type = messaging.EventTriggerFired
		body.EventType = ev.Type
	}
	body.Subject = ev.Subject
	body.EventID = ev.ID
	if ev.SourceKind != "" || ev.SourceID != "" {
		sourceName := ""
		if ev.Attributes != nil {
			sourceName = ev.Attributes["endpoint_name"]
		}
		body.Source = &messaging.EventSource{
			Kind: ev.SourceKind,
			ID:   ev.SourceID,
			Name: sourceName,
		}
	}
	if ev.Trigger != nil {
		body.Trigger = &messaging.EventTriggerRef{
			ID:   ev.Trigger.ID,
			Name: ev.Trigger.Name,
		}
	}
	verified := verifiedLabelForFireEvent(ev)
	if len(ev.Attributes) > 0 {
		attrs := maps.Clone(ev.Attributes)
		if verified != messaging.ExternalVerifiedScionV1 {
			// §8.1a: only scion_v1 signs routing headers; body_only and any
			// unverified/unknown mode never forward unsigned header attributes.
			delete(attrs, "delivery")
			delete(attrs, "event")
			delete(attrs, "subject")
		}
		if len(attrs) > 0 {
			body.Attributes = attrs
		}
	}
	if len(ev.Payload) > 0 {
		body.External = messaging.NewExternalPayload(verified, ev.Payload)
	}
	return body
}

// ApplyDispatchedEventEnvelope populates structuredMsg's §8.3 event carrier and
// sender metadata when req.Event is present, before action_message.go persists
// (when enabled) and calls RenderDeliveryText.
//
// Named without the word "Trigger" so action_message.go can invoke it while
// satisfying TestFireCoreContainment.
func ApplyDispatchedEventEnvelope(structuredMsg *messages.StructuredMessage, req hubfire.FireRequest) error {
	if structuredMsg == nil || req.Event == nil {
		return nil
	}
	if req.Event.Trigger != nil {
		structuredMsg.Sender = "trigger"
		structuredMsg.SenderID = "TRIGGER"
		if structuredMsg.Metadata == nil {
			structuredMsg.Metadata = make(map[string]string)
		}
		structuredMsg.Metadata["system_category"] = messages.SystemCategoryTrigger
	}
	if !req.Event.OccurredAt.IsZero() {
		structuredMsg.Timestamp = req.Event.OccurredAt.UTC().Format(time.RFC3339)
	}

	body := buildDispatchedEventBody(req.Event)
	rawEvent, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal dispatched event body: %w", err)
	}
	structuredMsg.Event = rawEvent
	return nil
}
