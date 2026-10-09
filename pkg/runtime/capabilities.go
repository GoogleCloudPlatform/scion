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
	"context"
	"errors"
)

// ErrLogsNotSupported is returned by GetLogs when a runtime cannot serve
// per-agent logs at all — for example because the underlying platform has no
// per-actor log RPC, or because logs are only readable at a level shared
// across tenants and would leak another agent's output. runtimebroker maps
// this, via errors.Is, to a fixed 501 response, so a wrapped error's own
// text never reaches a caller in its place. A runtime that supports logs
// never returns this.
var ErrLogsNotSupported = errors.New("agent logs are not available on this runtime")

// RecordlessActor identifies one actor a RecordlessActorProber found running
// with no in-memory record of which request created it — for example, after
// the runtime's own process restarted. UID should be a globally unique,
// backend-assigned identifier (not derived from Name alone), so a caller
// checking more than one runtime instance can dedupe by actor identity
// rather than by a name that can collide across instances.
type RecordlessActor struct {
	Name string
	UID  string
}

// RecordlessActorProber is an optional capability a Runtime may implement
// when it cannot always tell a project-scoped caller "not found" apart from
// "this process lost the record that would prove it" — a runtime whose
// actors carry no labels the broker can query after a restart is one
// example. RecordlessActors reports the runtime's own scope
// for projectID (for example, a namespace) and the names of any actor in it
// with no such record, so a caller can turn a would-be not-found into an
// explicit, distinguishable error instead of an idempotent success that
// would silently orphan the actor.
//
// A runtime that always keeps an authoritative record of what it is running
// does not need to implement this interface; the type assertion simply
// fails for it, and its callers' behavior is unchanged.
type RecordlessActorProber interface {
	RecordlessActors(ctx context.Context, projectID string) (scope string, actors []RecordlessActor, err error)
}

// PerProfileInstancesRuntime is an optional capability a Runtime may
// implement to say that its instances are bound to a specific profile's
// configuration, so a request naming a different profile of the same
// runtime type needs its own manager rather than the one already built for
// the default runtime. For most runtimes a matching type means the same
// backend and the default manager can serve any profile of that type; a
// runtime whose instance carries profile-specific settings (for example a
// per-profile egress policy) cannot be shared that way.
//
// A runtime that does not implement this interface, or reports false, keeps
// the type-only behavior: a profile resolving to the default runtime's type
// is served by the default manager.
type PerProfileInstancesRuntime interface {
	PerProfileInstances() bool
}

// HasPerProfileInstances reports whether rt implements
// PerProfileInstancesRuntime and reports true.
func HasPerProfileInstances(rt Runtime) bool {
	pp, ok := rt.(PerProfileInstancesRuntime)
	return ok && pp.PerProfileInstances()
}

// AttachCapableRuntime is an optional capability a Runtime may implement to
// report whether it supports interactive attach — a PTY or exec primitive a
// caller can dial to reach an actor's running session. Attach has always
// been available before this capability existed, so a runtime that does not
// implement this interface is treated as supporting it; only a runtime that
// explicitly lacks the primitive (for example, one whose broker rejects the
// PTY stream outright) needs to opt out by implementing this and reporting
// false.
type AttachCapableRuntime interface {
	SupportsAttach() bool
}

// HasAttachSupport reports whether rt supports interactive attach: true
// unless rt implements AttachCapableRuntime and reports false. A runtime
// that doesn't implement the interface — the common case — is supported by
// default (missing capability ⇒ supported).
func HasAttachSupport(rt Runtime) bool {
	ac, ok := rt.(AttachCapableRuntime)
	return !ok || ac.SupportsAttach()
}

// AsyncLaunchUnsupportedRuntime is an optional capability a Runtime may
// implement to report that it cannot serve an asynchronous launch. An async
// launch depends on the runtime calling RunConfig.Checkpoint and
// RunConfig.OnResourceCreated from Run, so the broker can cancel the launch
// at a checkpoint and clean up the resources the runtime reported. A runtime
// that does not call those hooks reports true here, and the broker falls
// back to a synchronous create for it.
//
// A runtime that does not implement this interface is treated as supporting
// async launch (missing capability ⇒ supported).
type AsyncLaunchUnsupportedRuntime interface {
	AsyncLaunchUnsupported() bool
}

// HasAsyncLaunchSupport reports whether rt can serve an asynchronous
// launch: true unless rt implements AsyncLaunchUnsupportedRuntime and
// reports true.
func HasAsyncLaunchSupport(rt Runtime) bool {
	au, ok := rt.(AsyncLaunchUnsupportedRuntime)
	return !ok || !au.AsyncLaunchUnsupported()
}

// EmptyPerAgentCapableRuntime is an optional capability a Runtime may
// implement to report whether it can run an empty-per-agent workspace
// (design #2703): a private, non-git per-agent directory on the node or pod.
// A runtime that does not implement it is treated as supporting the mode;
// one that cannot (Cloud Run, which rejects the mode at Run) opts out by
// implementing this and reporting false.
type EmptyPerAgentCapableRuntime interface {
	SupportsEmptyPerAgentWorkspace() bool
}

// HasEmptyPerAgentSupport reports whether rt supports empty-per-agent
// workspaces: true unless rt implements EmptyPerAgentCapableRuntime and
// reports false. Brokers advertise it per default runtime, like Attach.
func HasEmptyPerAgentSupport(rt Runtime) bool {
	ec, ok := rt.(EmptyPerAgentCapableRuntime)
	return !ok || ec.SupportsEmptyPerAgentWorkspace()
}

// AgentResourceCleaner is an optional capability a Runtime may implement
// when it creates per-agent objects alongside the container (for example
// the Kubernetes runtime's per-agent Secrets and SecretProviderClass).
// Delete removes those objects together with the container, but a delete
// only reaches Delete when the container is still listed. When the container
// was already removed outside scion (a preempted or evicted pod, for
// example), the agent delete calls CleanupAgentResources instead so those
// objects do not stay behind.
//
// agentName is the agent slug (the scion.name label) and projectID the
// agent's project ID (the scion.project_id label). Implementations must
// select objects by both, never by name alone, and must leave alone the
// objects of an agent whose container still exists. A projectID of ""
// is a no-op: without a project there is no safe scope.
//
// runID is the run the delete names (ptone/scion#2550). When it is set,
// implementations must select only that run's objects (the scion.run_id
// label equal to runID) and legacy objects carrying no run label, never an
// object labelled with another run: a delete naming an older run must not
// remove the objects a newer run's start has created before its container
// exists. An empty runID (a delete naming no run) selects by name and
// project, as before run IDs existed.
//
// Known limitation: without a runID there is no incarnation check. A start
// of the same agent still in progress (its objects created, its container
// not yet) could lose its objects; the worst case is that start failing.
// The broker cancels local launches of the agent before a delete resolves.
type AgentResourceCleaner interface {
	CleanupAgentResources(ctx context.Context, agentName, projectID, runID string) error
}
