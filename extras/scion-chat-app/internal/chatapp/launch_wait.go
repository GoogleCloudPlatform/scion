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

package chatapp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// Following agents that are still launching (ptone/scion#4671).
//
// A Hub with async launch answers a create that opted in (hub.async_create)
// as soon as the broker accepts it, with the agent still launching, and it
// refuses a message to a launching agent (409 agent_not_running). So the
// chat app answers the command at once and follows the launch in the
// background: one watcher goroutine per launching agent waits for the
// launch (hubclient.WaitForLaunch, with the client of the user whose
// command started the watch: for /create and /thread, the identity that
// made the create) and then works through the agent's queue of deferred
// work in order: the create's outcome post, the /thread instruction, and
// user messages relayed while the agent was starting. A launch that does
// not reach running drops the queued messages and posts the failure once
// per space and thread.
//
// The watchers run detached from the chat event (the HTTP ingress has a
// short deadline), bounded by the wait budget and by the router's
// lifetime: Shutdown stops them, and nothing is posted after that. A
// restart loses pending work (design 4.5, accepted).

// maxDeferredPerAgent bounds the queue of work waiting for one launching
// agent.
const maxDeferredPerAgent = 20

// deferredSendTimeout bounds each send or post made after the wait.
const deferredSendTimeout = 30 * time.Second

// errDeferredQueueFull is returned by deferUntilRunning when the agent's
// queue is full.
var errDeferredQueueFull = errors.New("too many messages are already waiting for this agent to start")

// deferredWork is one piece of work waiting for an agent's launch to end.
type deferredWork struct {
	// spaceID and threadID are where the failure is posted; place, when
	// set, replaces them and is called after ready.
	spaceID  string
	threadID string
	place    func() (spaceID, threadID string)
	// ready, when set, is closed by the command once the work can run: an
	// item is registered early (so later work queues behind it) and its
	// run, failed and place are not used before ready is closed.
	ready <-chan struct{}
	// message is true when the work delivers a user message; the failure
	// post counts the messages that were not delivered.
	message bool
	// run is called, in queue order, once the agent is running. It sends
	// and posts its own result.
	run func(ctx context.Context)
	// failed, when set, is called when the launch did not reach running,
	// before the failure is posted (for example to update a card). It
	// must not post the failure text itself.
	failed func(ctx context.Context, text string)
}

// agentLaunchQueue is the deferred work of one launching agent.
type agentLaunchQueue struct {
	items []deferredWork
}

// initLaunchWatch sets up the router's background context. It is called
// lazily so routers built in tests without NewCommandRouter work too.
func (r *CommandRouter) initLaunchWatch() {
	r.bgOnce.Do(func() {
		r.bgCtx, r.bgCancel = context.WithCancel(context.Background())
		if r.launchQueues == nil {
			r.launchQueues = make(map[string]*agentLaunchQueue)
		}
	})
}

// Shutdown stops the launch watchers and waits for them to return, or for
// ctx to end. Pending deferred work is dropped without being posted. It
// may be called more than once.
func (r *CommandRouter) Shutdown(ctx context.Context) error {
	r.initLaunchWatch()
	// Cancel under launchMu, so no watcher is added (bg.Add in
	// deferUntilRunning, after its bgCtx check) once Wait may have started.
	r.launchMu.Lock()
	r.bgCancel()
	r.launchMu.Unlock()
	done := make(chan struct{})
	go func() {
		r.bg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// launchQueueKey identifies an agent within a project.
func launchQueueKey(projectID, agent string) string {
	return projectID + "/" + agent
}

// agentQueueKey returns the queue key for a: its ID, so an agent deleted
// while launching and re-created with the same name gets its own queue. It
// falls back to ref (the slug or ID the user gave) when a is nil or has no
// ID.
func agentQueueKey(projectID, ref string, a *hubclient.Agent) string {
	if a != nil && a.ID != "" {
		return launchQueueKey(projectID, a.ID)
	}
	return launchQueueKey(projectID, ref)
}

// deferUntilRunning queues w behind the agent's launch. It joins the
// agent's queue when one exists (the agent is still launching, or earlier
// deferred work has not run yet), so work runs in the order it arrived.
// Otherwise, when a shows an active launch, it starts a watcher that waits
// for the launch with svc. It returns false, with no error, when the agent
// is not launching and nothing is queued: the caller sends at once. It
// returns errDeferredQueueFull when the queue is full, and the context
// error after Shutdown.
func (r *CommandRouter) deferUntilRunning(key string, svc hubclient.AgentGetter, a *hubclient.Agent, w deferredWork) (bool, error) {
	r.initLaunchWatch()
	r.launchMu.Lock()
	defer r.launchMu.Unlock()
	if err := r.bgCtx.Err(); err != nil {
		return false, err
	}
	if q := r.launchQueues[key]; q != nil {
		if len(q.items) >= maxDeferredPerAgent {
			return false, errDeferredQueueFull
		}
		q.items = append(q.items, w)
		return true, nil
	}
	// a may be nil (the respond relay's read failed): LaunchActive is
	// nil-safe and reports false, so the caller sends at once.
	if !hubclient.LaunchActive(a) {
		return false, nil
	}
	r.launchQueues[key] = &agentLaunchQueue{items: []deferredWork{w}}
	r.bg.Add(1)
	go r.watchLaunch(key, svc, a)
	return true, nil
}

// watchLaunch waits for a's launch and then drains the agent's queue.
func (r *CommandRouter) watchLaunch(key string, svc hubclient.AgentGetter, a *hubclient.Agent) {
	defer r.bg.Done()
	ctx := r.bgCtx
	id := a.ID
	if id == "" {
		id = a.Slug
	}
	out, err := hubclient.WaitForLaunch(ctx, svc, id, a, hubclient.LaunchWaitOptions{PollInterval: r.launchPollInterval})
	if ctx.Err() != nil {
		r.dropLaunchQueue(key)
		return
	}
	if err == nil && out.Running {
		r.drainLaunchQueue(ctx, key, func(w deferredWork) {
			w.run(ctx)
		})
		return
	}
	name := a.Slug
	if name == "" {
		name = a.Name
	}
	text := launchFailureText(name, out, err)
	// Collect the whole queue, including work queued while the failure is
	// handled, and post the failure once per place.
	var failed []deferredWork
	r.drainLaunchQueue(ctx, key, func(w deferredWork) {
		failed = append(failed, w)
	})
	r.postLaunchFailure(ctx, failed, text)
}

// drainLaunchQueue calls fn for each queued item in order, including items
// queued meanwhile, and removes the queue once it is empty. It stops,
// dropping the rest, when ctx ends.
func (r *CommandRouter) drainLaunchQueue(ctx context.Context, key string, fn func(deferredWork)) {
	for {
		r.launchMu.Lock()
		q := r.launchQueues[key]
		if q == nil || len(q.items) == 0 || ctx.Err() != nil {
			delete(r.launchQueues, key)
			r.launchMu.Unlock()
			return
		}
		w := q.items[0]
		q.items = q.items[1:]
		r.launchMu.Unlock()
		if w.ready != nil {
			select {
			case <-w.ready:
			case <-ctx.Done():
				continue // the next pass drops the rest
			}
		}
		fn(w)
	}
}

// dropLaunchQueue removes the agent's queue without running it.
func (r *CommandRouter) dropLaunchQueue(key string) {
	r.launchMu.Lock()
	delete(r.launchQueues, key)
	r.launchMu.Unlock()
}

// postLaunchFailure runs each item's failed hook and posts text once per
// space and thread, with the number of messages that were not delivered.
func (r *CommandRouter) postLaunchFailure(ctx context.Context, items []deferredWork, text string) {
	type place struct{ space, thread string }
	var order []place
	dropped := make(map[place]int)
	for _, w := range items {
		if ctx.Err() != nil {
			return
		}
		if w.failed != nil {
			w.failed(ctx, text)
		}
		p := place{w.spaceID, w.threadID}
		if w.place != nil {
			p.space, p.thread = w.place()
		}
		if _, seen := dropped[p]; !seen {
			order = append(order, p)
			dropped[p] = 0
		}
		if w.message {
			dropped[p]++
		}
	}
	for _, p := range order {
		msg := text
		switch n := dropped[p]; n {
		case 0:
		case 1:
			msg += "\n1 message was not delivered."
		default:
			msg += fmt.Sprintf("\n%d messages were not delivered.", n)
		}
		r.postDeferred(ctx, p.space, p.thread, msg)
	}
}

// postDeferred posts text from a watcher. Nothing is posted after
// Shutdown.
func (r *CommandRouter) postDeferred(ctx context.Context, spaceID, threadID, text string) {
	if ctx.Err() != nil {
		return
	}
	postCtx, cancel := context.WithTimeout(ctx, deferredSendTimeout)
	defer cancel()
	if _, err := r.messenger.SendMessage(postCtx, SendMessageRequest{
		SpaceID:  spaceID,
		ThreadID: threadID,
		Text:     text,
	}); err != nil {
		r.log.Warn("launch follow-up: posting failed", "error", err, "space", spaceID)
	}
}

// launchFailureText describes why the wait for agent name's launch ended
// without the agent running.
func launchFailureText(name string, out hubclient.LaunchOutcome, err error) string {
	switch {
	case err == nil:
		label := out.Code
		if label == "" {
			label = "phase: " + out.Phase
		}
		text := fmt.Sprintf("Agent `%s` failed to start (%s)", name, label)
		if out.Message != "" {
			text += ": " + out.Message
		}
		return text
	case errors.Is(err, hubclient.ErrLaunchWaitTimeout):
		return fmt.Sprintf("Agent `%s` is still starting; I stopped waiting. Check `/scionAdmin status %s`.", name, name)
	case errors.Is(err, hubclient.ErrAgentDeletedDuringLaunch):
		return fmt.Sprintf("Agent `%s` was deleted while it was starting.", name)
	default:
		return fmt.Sprintf("Could not confirm that agent `%s` started: %v", name, err)
	}
}

// launchStartingNotice is the reply to a message relayed to an agent that
// is still starting.
func launchStartingNotice(agent string) string {
	return fmt.Sprintf("Agent `%s` is still starting; your message will be delivered when it is running.", agent)
}

// launchQueueFullText is the reply when an agent's queue is full.
func launchQueueFullText(agent string) string {
	return fmt.Sprintf("Agent `%s` is still starting and %d messages are already waiting for it; send yours again once it is running.", agent, maxDeferredPerAgent)
}

// deferredContext returns a context for one send or post made after the
// wait.
func deferredContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, deferredSendTimeout)
}
