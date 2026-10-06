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

package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

type Manager interface {
	// Provision prepares the agent directory and configuration without starting it
	Provision(ctx context.Context, opts api.StartOptions) (*api.ScionConfig, error)

	// Reprovision re-renders an existing agent's on-disk configuration from
	// the current template/harness-config catalog (a `scion reincarnate`
	// request), preserving its home directory and clone-per-agent workspace.
	// See AgentManager.Reprovision for the full contract.
	Reprovision(ctx context.Context, opts api.StartOptions) (*api.ScionConfig, error)

	// Preflight resolves opts' template and harness config without
	// provisioning, cloning, writing files or calling the runtime (design
	// t1-async-create-v11.md §3.1 "Admission", §7 P1b-1). It returns
	// config.ErrTemplateNotFound or config.ErrHarnessConfigNotFound exactly
	// as Provision/Start would, so an async create's admission phase can
	// surface those synchronously before accepting the launch.
	Preflight(ctx context.Context, opts api.StartOptions) error

	// CleanupLaunch deletes the runtime resources an aborted launch created
	// (design §3.8.4). See AgentManager.CleanupLaunch for the UID-precondition
	// and fresh-context contract.
	CleanupLaunch(ctx context.Context, handles []ResourceHandle) error

	// Start launches a new agent with the given configuration
	Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error)

	// Stop terminates an agent
	Stop(ctx context.Context, agentID string, projectPath string) error

	// Delete terminates and removes an agent, resolving agentID by slug
	// (scoped by projectPath when given). It fails closed when the slug is
	// ambiguous.
	Delete(ctx context.Context, agentID string, deleteFiles bool, projectPath string, removeBranch bool) (bool, error)

	// DeleteTarget terminates and removes an agent the caller has already
	// resolved to a specific runtime entry (ref.ID, may be empty for a
	// file-only agent; ref.RunID is that entry's run ID) and project path.
	// It never re-resolves by slug.
	DeleteTarget(ctx context.Context, agentName string, ref runtime.RunRef, deleteFiles bool, projectPath string, removeBranch bool) (bool, error)

	// List returns active agents
	List(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error)

	// Message sends a message to an agent's harness via tmux.
	// projectID scopes delivery to a specific project, preventing cross-project
	// collision when agents share the same slug.
	Message(ctx context.Context, agentID, projectID string, message string, interrupt bool) error

	// SendKeys sends the exact byte-for-byte keys string to an agent's tmux
	// session via one generated "send-keys ... -- <keys>" command, delivered
	// on stdin to "tmux source-file -" (requires tmux ≥ 3.1) — the frozen
	// primitive for the dedicated broker /keys route
	// (.design/agent-keys-contract.md §4.3, §2.3). It binds to the
	// resolved container's "agent_id" label: it resolves the
	// target by (projectID, agentSlug), verifies the resolved container's
	// "agent_id" label equals expectedAgentID, and executes on that same
	// resolved container, all within this one call — see
	// AgentManager.SendKeys's doc comment for why that atomicity matters.
	//
	// Return classes (see §4.3's return/mapping table and
	// AgentManager.SendKeys's doc comment for why each one exists):
	// agentkeys.ErrTargetNotFound, ErrAgentNotRunning or ErrTerminalNotReady
	// when it can prove the corresponding condition; a
	// *agentkeys.ValidationError for a malformed/oversized/empty keys value,
	// checked before anything else; the package-local ErrKeysUnsupported
	// when the backend does not support keys delivery (including a tmux
	// below the version floor); an error wrapping the package-local
	// ErrKeysNotStarted for a failure proven to occur before delivery began;
	// or, for any other failure (including one that itself wraps a context
	// error), a plain error that must never be mistaken for one of the
	// above.
	SendKeys(ctx context.Context, projectID, agentSlug, expectedAgentID, keys string) error

	// SendKeysLocal is SendKeys's additive local-scope sibling for a project
	// never linked to a Hub project ID (ptone/scion#2198/#2468 finding 3):
	// same delivery core, scoped by the resolved local project-config
	// directory path instead. See AgentManager.SendKeysLocal's doc comment.
	SendKeysLocal(ctx context.Context, projectPath, agentSlug, expectedAgentID, keys string) error

	// Watch returns a channel of status updates for an agent
	Watch(ctx context.Context, agentID string) (<-chan api.StatusEvent, error)

	// Close flushes any pending message buffers. Must be called before
	// the process exits to ensure buffered messages are delivered.
	Close()
}

type AgentManager struct {
	Runtime   runtime.Runtime
	msgBuffer *MessageBuffer

	// injectionLocks holds one *injectionMutex per resolved container ID,
	// lazily created by injectionLock. It serializes tmux injection
	// (message paste/interrupt and SendKeys) for the same
	// target so their byte sequences cannot interleave — see
	// injectionLock's doc comment. Entries are never removed: each one is a
	// small, fixed-size mutex, not a store of message content, so retaining
	// one per container ID ever seen for the process's lifetime is an
	// acceptable trade against the complexity of reference-counted eviction.
	injectionLocks sync.Map

	// tmuxVersionOK caches, per tmuxVersionCacheKey, that
	// checkTmuxVersionSupported has already determined that container's tmux
	// meets SendKeys's minimum version — see that method's doc comment for
	// why only a positive, successfully-parsed result is ever cached, and
	// tmuxVersionCacheKey's doc comment for why the key is not ContainerID
	// alone.
	tmuxVersionOK sync.Map
}

// defaultBufferDelay is the debounce window for message delivery.
// Messages arriving within this window are coalesced into a single delivery.
const defaultBufferDelay = 2 * time.Second

// msgBufferPrefix is the base name for the named tmux buffer used to load
// message text via stdin before pasting it into an agent's session. Using a
// named buffer (rather than the default buffer) avoids clobbering unrelated
// tmux buffer usage and lets paste-buffer delete it immediately after use
// (-d). Each delivery appends a unique suffix (see nextMsgBufferName) so that
// two deliveries to the same agent overlapping in time — an interrupt racing
// a buffered flush, or two retries — never share a buffer: with a single
// fixed name, one delivery's paste can consume the buffer loaded for the
// other, silently swapping which message reaches the terminal and which
// delivery is reported as failed (ptone/scion#2265).
const msgBufferPrefix = "scion-msg"

// msgBufferSeq generates the per-delivery suffix for msgBufferPrefix. A
// counter alone is only unique within the process that owns it: every
// AgentManager in one broker process shares this package-level counter, so
// it tells their deliveries apart, but a short-lived CLI invocation (scion
// message, scion broadcast) starts a fresh process whose counter restarts at
// 1, so two such processes — two concurrent CLI calls, or a CLI call racing
// the broker, against the same agent — would otherwise both name their first
// delivery "scion-msg-1". msgBufferNonce below supplies the part that tells
// processes apart; the counter only needs to tell apart deliveries within
// one process.
var msgBufferSeq atomic.Uint64

// msgBufferNonce is a random value generated once per process (not once per
// call) so that every buffer name that process produces carries it. It is
// what makes nextMsgBufferName unique across processes: two processes each
// generate their own nonce independently, so their names never collide even
// though each process's msgBufferSeq counter restarts at 1. A PID is not
// enough for this, since containerized brokers can share PID 1.
var msgBufferNonce = sync.OnceValue(func() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is effectively unrecoverable on any supported
		// platform; fail loudly rather than silently falling back to a
		// predictable, collision-prone name.
		panic(fmt.Sprintf("agent: reading random buffer-name nonce: %v", err))
	}
	return hex.EncodeToString(b[:])
})

// nextMsgBufferName returns a short, unique tmux buffer name for a single
// deliverImmediate call. It must be computed once per call and reused for
// every step of that call (load-buffer, paste-buffer, and any cleanup),
// never regenerated mid-delivery.
func nextMsgBufferName() string {
	return fmt.Sprintf("%s-%s-%d", msgBufferPrefix, msgBufferNonce(), msgBufferSeq.Add(1))
}

// loadBufferArgv builds the argv that loads message text into the named tmux
// buffer bufName via stdin. Shared by deliverImmediate and the real-tmux
// integration test so the test exercises the exact argv delivery uses.
func loadBufferArgv(bufName string) []string {
	return []string{"tmux", "load-buffer", "-b", bufName, "-"}
}

// pasteBufferArgv builds the argv that pastes the named tmux buffer bufName
// into target with bracketed paste, deleting the buffer on success. Shared by
// deliverImmediate and the real-tmux integration test so the test exercises
// the exact argv delivery uses.
func pasteBufferArgv(target, bufName string) []string {
	return []string{"tmux", "paste-buffer", "-t", target, "-p", "-d", "-b", bufName}
}

func NewManager(rt runtime.Runtime) Manager {
	mgr := &AgentManager{
		Runtime: rt,
	}
	// Initialize the message buffer with a debounce delay. The buffer's
	// delivery function calls back into deliverImmediate to perform the
	// actual tmux send-keys when the debounce window expires.
	mgr.msgBuffer = NewMessageBuffer(defaultBufferDelay, func(agentID, projectID string, message string, interrupt bool) error {
		return mgr.deliverImmediate(context.Background(), agentID, projectID, message, interrupt)
	})
	return mgr
}

func (m *AgentManager) Close() {
	m.msgBuffer.Close()
}

// resolveProjectName maps a project path to the project name used to scope a
// slug lookup. It returns "" when no path is given or it cannot be resolved.
func resolveProjectName(projectPath string) string {
	if projectPath == "" {
		return ""
	}
	if resolvedDir, err := config.GetResolvedProjectDir(projectPath); err == nil {
		return config.GetProjectName(resolvedDir)
	}
	return ""
}

// agentHasProjectInfo reports whether a runtime entry carries any project
// identity (label or field) that can be compared against a requested project.
func agentHasProjectInfo(a api.AgentInfo) bool {
	return projectkeys.ProjectIDFromLabels(a.Labels) != "" ||
		projectkeys.ProjectNameFromLabels(a.Labels) != "" ||
		a.ProjectID != "" || a.Project != ""
}

// selectAgentTarget picks the single runtime entry that agentID refers to.
//
// When projectName is set, entries that carry project info must match it;
// entries without any project info are accepted only when no entry with
// matching project info exists (backward compatibility with pre-label
// containers). It never falls back to an entry that is labelled for a
// different project.
//
// It fails closed with an error when more than one distinct entry remains,
// instead of silently acting on whichever one the runtime listed first
// (ptone/scion#1819). found is false when nothing matches.
func selectAgentTarget(agents []api.AgentInfo, agentID, projectName string) (target api.AgentInfo, found bool, err error) {
	var scoped, unlabeled []api.AgentInfo
	for _, a := range agents {
		if !agentMatchesName(a, agentID) {
			continue
		}
		if projectName == "" {
			scoped = append(scoped, a)
			continue
		}
		if !agentHasProjectInfo(a) {
			unlabeled = append(unlabeled, a)
			continue
		}
		if matchAgentProject(a, projectName, "") {
			scoped = append(scoped, a)
		}
	}
	candidates := scoped
	if len(candidates) == 0 {
		candidates = unlabeled
	}
	candidates = DedupeByContainerID(candidates)
	switch len(candidates) {
	case 0:
		return api.AgentInfo{}, false, nil
	case 1:
		return candidates[0], true, nil
	default:
		return api.AgentInfo{}, false, fmt.Errorf("agent '%s' is ambiguous: %d containers match; specify the project", agentID, len(candidates))
	}
}

// agentMatchesName reports whether a runtime entry refers to agentID by name
// (case-insensitively) or container ID.
func agentMatchesName(a api.AgentInfo, agentID string) bool {
	return a.Name == agentID || a.ContainerID == agentID ||
		strings.TrimPrefix(a.Name, "/") == agentID ||
		strings.EqualFold(a.Name, agentID)
}

// DedupeByContainerID collapses duplicate listings of the same container
// (some runtime backends can report an entry more than once for a single
// container) down to one entry per container identity, keyed by
// ContainerID when present, falling back to a composite of name/project
// fields for an entry with no container (e.g. a "created" but not yet
// started agent). Exported so callers outside this package (e.g. CLI
// target-resolution code) that need the exact same de-duplication rule
// selectAgentTarget and resolveKeysTarget already apply internally do not
// need to keep a second copy of it.
func DedupeByContainerID(agents []api.AgentInfo) []api.AgentInfo {
	if len(agents) < 2 {
		return agents
	}
	seen := make(map[string]bool, len(agents))
	out := make([]api.AgentInfo, 0, len(agents))
	for _, a := range agents {
		key := a.ContainerID
		if key == "" {
			key = "name:" + a.Name + "|" + a.ProjectID + "|" + a.Project
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, a)
	}
	return out
}

func (m *AgentManager) Stop(ctx context.Context, agentID string, projectPath string) error {
	// Resolve the agent name to a container ID so that runtimes which do
	// not support lookup-by-name (e.g. Apple's `container` CLI) receive
	// the actual container ID.  This mirrors the resolution logic in Delete().
	slug := api.Slugify(agentID)
	agents, err := m.Runtime.List(ctx, map[string]string{"scion.name": slug})
	if err == nil {
		target, found, selErr := selectAgentTarget(agents, agentID, resolveProjectName(projectPath))
		if selErr != nil {
			return selErr
		}
		if found {
			return m.Runtime.Stop(ctx, runtime.AgentOperationID(target))
		}
	}
	// Fallback: agentID may already be a container ID, or the list
	// failed — pass it through directly.
	return m.Runtime.Stop(ctx, agentID)
}

func (m *AgentManager) Delete(ctx context.Context, agentID string, deleteFiles bool, projectPath string, removeBranch bool) (bool, error) {
	// 1. Check if container exists
	// We use name filter if possible, but runtime.List might take map[string]string
	util.Debugf("delete: listing containers in mgr.Delete for %s", agentID)
	listStart := time.Now()
	slug := api.Slugify(agentID)
	agents, err := m.Runtime.List(ctx, map[string]string{"scion.name": slug})
	util.Debugf("delete: mgr.Delete container list completed in %v", time.Since(listStart))
	var target runtime.RunRef
	if err == nil {
		// Resolve project name from projectPath (if provided) to scope the
		// container lookup; refuse ambiguous matches rather than picking one.
		entry, found, selErr := selectAgentTarget(agents, agentID, resolveProjectName(projectPath))
		if selErr != nil {
			return false, selErr
		}
		if found {
			target = runtime.RunRef{ID: runtime.AgentOperationID(entry), RunID: entry.RunID}
		}
	}
	return m.deleteResolved(ctx, agentID, target, deleteFiles, projectPath, removeBranch)
}

// ErrRuntimeDelete wraps a failure of the runtime Delete call in a delete
// (deleteResolved). It tells that failure, after which the runtime entry
// may still exist, apart from a later file-cleanup failure, which comes
// after a successful runtime delete.
var ErrRuntimeDelete = errors.New("failed to delete container")

// DeleteTarget deletes an agent that the caller has already resolved to a
// specific runtime entry. Unlike Delete it performs no slug re-resolution, so
// it cannot drift to a same-slug agent in another project. ref.ID may be
// empty for an agent that has files but no backing container; ref.RunID is
// the resolved entry's run ID and is passed through to Runtime.Delete.
// projectPath is used verbatim for file deletion.
func (m *AgentManager) DeleteTarget(ctx context.Context, agentName string, ref runtime.RunRef, deleteFiles bool, projectPath string, removeBranch bool) (bool, error) {
	return m.deleteResolved(ctx, agentName, ref, deleteFiles, projectPath, removeBranch)
}

func (m *AgentManager) deleteResolved(ctx context.Context, agentName string, ref runtime.RunRef, deleteFiles bool, projectPath string, removeBranch bool) (bool, error) {
	if targetID := ref.ID; targetID != "" {
		// Stop the container gracefully before force-removing it. This ensures
		// bind mounts (e.g. shared-dir volumes) are properly released before
		// filesystem cleanup. Without this, docker rm -f / container kill sends
		// SIGKILL which can leave mounts in a state that causes permission
		// errors when DeleteAgentFiles tries to remove the agent directory.
		util.Debugf("delete: stopping container %s before removal", targetID)
		if err := m.Runtime.Stop(ctx, targetID); err != nil {
			// Log but don't fail — the container may already be stopped,
			// and Delete (force-remove) will handle it either way.
			util.Debugf("delete: stop returned error (continuing): %v", err)
		}

		util.Debugf("delete: starting runtime delete for container %s (run_id=%q)", targetID, ref.RunID)
		if err := m.Runtime.Delete(ctx, ref); err != nil {
			return false, fmt.Errorf("%w: %w", ErrRuntimeDelete, err)
		}
		util.Debugf("delete: runtime delete completed for container %s", targetID)
	}

	if deleteFiles {
		util.Debugf("delete: starting filesystem cleanup for agent %s", agentName)
		branchDeleted, err := DeleteAgentFiles(agentName, projectPath, removeBranch)
		util.Debugf("delete: filesystem cleanup completed for agent %s", agentName)
		return branchDeleted, err
	}
	return false, nil
}

// CleanupAgentResources removes runtime objects that an agent's start
// created beside its container (for example Kubernetes Secrets) when the
// container itself is already gone, so deleteResolved never reaches
// Runtime.Delete. It is a no-op for a runtime that does not implement
// runtime.AgentResourceCleaner. See that interface for the scoping rules.
func (m *AgentManager) CleanupAgentResources(ctx context.Context, agentName, projectID string) error {
	if c, ok := m.Runtime.(runtime.AgentResourceCleaner); ok {
		return c.CleanupAgentResources(ctx, agentName, projectID)
	}
	return nil
}

func (m *AgentManager) Watch(ctx context.Context, agentID string) (<-chan api.StatusEvent, error) {
	return nil, fmt.Errorf("Watch not implemented")
}

func (m *AgentManager) Message(ctx context.Context, agentID, projectID string, message string, interrupt bool) error {
	// Interrupt messages bypass the buffer entirely — they need to send
	// Ctrl+C immediately to get the agent's attention, and the accompanying
	// message (if any) should follow without delay.
	if interrupt {
		return m.deliverImmediate(ctx, agentID, projectID, message, interrupt)
	}

	// Before buffering, make sure the target has a running container. A
	// buffered message is reported as accepted at once and only fails later,
	// asynchronously, so without this check a message to an agent whose
	// container is gone (for example a Kubernetes pod removed by a node
	// drain) would look delivered to the sender. Only a definite answer from
	// the runtime fails the send; a lookup error falls back to the buffered
	// path so a transient runtime or API error never blocks delivery.
	if err := m.checkDeliveryTarget(ctx, agentID, projectID); err != nil {
		return err
	}

	// Non-interrupt messages go through the debounce buffer. This ensures
	// that a rapid burst of messages (e.g. from multiple senders or broadcast
	// fan-out) is coalesced into a single delivery, avoiding contention on
	// the agent's tmux input.
	// A failure handler on ctx (set by the runtime broker) is invoked if the
	// buffered delivery later fails, so the hub can mark the message failed
	// rather than leave it "dispatched" (#1820).
	m.msgBuffer.SendWithFailureHandler(agentID, projectID, message, DeliveryFailureHandlerFromContext(ctx))
	return nil
}

// errNoRunningContainer formats the error returned when a message target
// has no running container. It contains "not found" so the runtime broker
// maps it to 404, matching deliverImmediate's own lookup failure.
func errNoRunningContainer(agentID string) error {
	return fmt.Errorf("agent '%s' not found or not running", agentID)
}

// checkDeliveryTarget performs one scoped runtime lookup (the same name and
// project label filter deliverImmediate uses, so on Kubernetes a single
// label-selected pod list, on Docker a single filtered container list) and
// returns errNoRunningContainer only when the runtime answers definitively
// that the agent has no container, or that its container is stopped or
// errored. A lookup error, or a container whose state the runtime does not
// report, returns nil so the caller keeps the normal buffered path.
//
// It queries the runtime directly rather than m.List: the runtime's Phase is
// derived from the container state, while m.List merges the agent's
// self-reported agent-info.json phase on top of it.
func (m *AgentManager) checkDeliveryTarget(ctx context.Context, agentID, projectID string) error {
	if m.Runtime == nil {
		return nil
	}
	filter := map[string]string{"scion.name": strings.ToLower(agentID)}
	if projectID != "" {
		filter["scion.project_id"] = projectID
	}
	agents, err := m.Runtime.List(ctx, filter)
	if err != nil {
		slog.Warn("message target lookup failed; using buffered delivery",
			"agent", agentID, "project_id", projectID, "error", err)
		return nil
	}
	for _, a := range agents {
		if !matchesAgentID(a, agentID) {
			continue
		}
		switch state.Phase(a.Phase) {
		case state.PhaseStopped, state.PhaseError:
			// Keep looking: another matching container may be running.
		default:
			return nil
		}
	}
	// No matching container, or every matching container is stopped/errored.
	return errNoRunningContainer(agentID)
}

// RuntimeName returns the name of the runtime this manager lists, or "" when
// it has none. The broker heartbeat uses it to report which runtimes its
// agent inventory covers.
func (m *AgentManager) RuntimeName() string {
	if m.Runtime == nil {
		return ""
	}
	return m.Runtime.Name()
}

// keysTarget is the tmux target every injection primitive in this file
// addresses — the single window every agent harness runs in.
const keysTarget = "scion:0"

// injectionMutex is a per-target mutex whose Lock respects a context's
// deadline/cancellation, so a caller waiting for a target whose injection
// critical section is already held (a concurrent SendKeys, interrupt, or
// buffered flush for the same agent) does not block past its own admission
// deadline. Acquired via AgentManager.injectionLock; see that method's doc
// comment for what it serializes and why.
//
// Implemented as a 1-buffered channel holding a single token: Lock takes the
// token (or gives up when ctx is done first) and Unlock returns it. This
// avoids the need for a separate "acquired" flag or a busy-poll loop around
// sync.Mutex.TryLock, neither of which composes as directly with select on
// ctx.Done().
type injectionMutex struct {
	ch chan struct{}
}

func newInjectionMutex() *injectionMutex {
	im := &injectionMutex{ch: make(chan struct{}, 1)}
	im.ch <- struct{}{}
	return im
}

// Lock blocks until the mutex is free or ctx is done, whichever comes
// first. On the ctx-done path it returns ctx.Err() and acquires nothing —
// callers must not call Unlock in that case.
//
// An uncontended lock is always acquired immediately, even if ctx is
// already done: the non-blocking check below runs first, so this never
// depends on Go's random tie-break between two simultaneously ready select
// cases. Without it, a caller whose ctx happens to already be cancelled at
// the moment it calls Lock — e.g. an interrupt delivery racing the HTTP
// request that triggered it — could nondeterministically fail to acquire an
// otherwise free lock purely because select happened to pick the ctx.Done()
// case, even though nothing was actually contending for it (see
// TestDeliverImmediate_PasteFailureDeletesBuffer's already-cancelled
// callerCtx, which models exactly this and must still attempt delivery).
// Only a genuinely contended lock lets ctx cancellation preempt the wait.
func (im *injectionMutex) Lock(ctx context.Context) error {
	select {
	case <-im.ch:
		return nil
	default:
	}
	select {
	case <-im.ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Unlock releases the mutex. Must only be called after a successful Lock.
func (im *injectionMutex) Unlock() {
	im.ch <- struct{}{}
}

// injectionLock returns the per-target injectionMutex for containerID,
// creating it on first use. It never removes an entry: see
// AgentManager.injectionLocks's doc comment for why that is an acceptable
// trade for this type.
//
// Keyed by the resolved container ID rather than a caller-supplied
// agentID/slug string: both SendKeys and deliverImmediate already resolve
// exactly one container before injecting into it, so locking on that
// resolved identity makes the serialization guarantee independent of which
// spelling a caller used to name the target — a bare slug, a container ID,
// or a "/"-prefixed name all resolve to the same container and therefore
// the same lock, where two different string spellings previously could have
// raced with each other by accident. This ID is stable for a given running
// container, but on at least one backend it is a reusable name rather than
// a fresh identifier per instance (a Kubernetes pod name,
// pkg/runtime/k8s_runtime.go's AgentInfo.ContainerID assignment) — a later,
// different container recreated under that same name shares this lock with
// the one before it. That is harmless here (a mutex has no notion of which
// container it belongs to, only whether it is held), unlike
// checkTmuxVersionSupported's cache, which cannot use ContainerID alone for
// exactly this reason — see tmuxVersionCacheKey's doc comment.
//
// Serialization guarantee, stated precisely because it is easy to overstate:
// this lock only orders concurrent calls into this one AgentManager's
// deliverImmediate/SendKeys for the same resolved container. It
// says nothing about, and must not be relied on to order, interactive PTY
// input (a separate code path entirely, pkg/runtimebroker/pty_handlers.go)
// or a separate local CLI process's own manager instance (which has its own,
// independent injectionLocks map) — see
// .design/agent-keys-contract.md's "Execution and transport" section
// ("Interactive PTY input and separate local CLI processes/managers are not
// covered by this lock").
//
// A buffered message flush (MessageBuffer's deliverFunc) calls
// deliverImmediate with context.Background(), so a hung tmux call inside a
// flush can hold a target's lock indefinitely. This is fail-closed, not a
// deadlock: no other lock is held while a caller waits here, and
// MessageBuffer releases its own internal mutex before invoking deliverFunc
// (msgbuffer.go's flush) — but that retry loop runs only after deliverFunc
// itself returns, so it does nothing to bound this wait; a keys call
// contending for the same target is what stays bounded, failing closed with
// an ErrKeysNotStarted-wrapped error (translated by the runtimebroker
// handler to keys_unavailable) at its own (≤30s) deadline rather than
// blocking forever.
func (m *AgentManager) injectionLock(containerID string) *injectionMutex {
	if v, ok := m.injectionLocks.Load(containerID); ok {
		return v.(*injectionMutex)
	}
	actual, _ := m.injectionLocks.LoadOrStore(containerID, newInjectionMutex())
	return actual.(*injectionMutex)
}

// runtimeNamesWithoutKeysSupport lists Runtime.Name() values of backends
// that do not support keys delivery. Checked in SendKeys before any
// resolution or Exec attempt.
var runtimeNamesWithoutKeysSupport = map[string]bool{
	"cloudrun": true,
}

// ErrKeysUnsupported means the manager's underlying runtime backend does not
// support keys delivery — either because it is named in
// runtimeNamesWithoutKeysSupport (checked before any resolution or Exec
// attempt), or because the resolved target's tmux is below the version
// floor checkTmuxVersionSupported enforces (checked after resolution, the
// lock and one Exec call, but always before delivery began). The
// runtimebroker's dedicated keys handler translates this into
// OutcomeKeysUnsupported (422).
var ErrKeysUnsupported = errors.New("agent: this backend does not support keys delivery")

// ErrKeysNotStarted signals that a keys dispatch is proven to have failed
// strictly *before* delivery began: SendKeys wraps this (via %w, together
// with the ctx error that caused it) at each of its pre-delivery
// checkpoints — the lock wait, the post-lock recheck, a readiness probe
// failure that coincides with ctx expiry, a ctx expiry discovered when the
// pre-delivery target re-verification's own resolution fails, and the
// pre-send recheck — and nowhere else. "Before delivery began" is not the
// same as "before any
// Exec": the readiness probe is itself an Exec call, and a probe failure
// only produces this sentinel when it coincides with ctx expiry, never on
// its own (a genuinely unready terminal is ErrTerminalNotReady instead).
//
// This is a package-local sentinel, deliberately distinct from
// context.DeadlineExceeded/context.Canceled themselves, so that a caller
// (the runtimebroker handler) can recognize "proven not to have started" by
// identity (errors.Is(err, ErrKeysNotStarted)) without also matching a
// failure that happens to occur *after* the delivery call began and merely
// wraps a context error of its own — e.g. a backend's Exec implementation
// observing the same cancellation while genuinely running, which must
// classify as ambiguous (keys_outcome_unknown), never as "definitely did
// not start" (keys_unavailable): reporting that case as keys_unavailable
// would tell a caller it is safe to retry a dispatch that may have already
// reached the terminal, inviting a double injection. This is why the
// delivery call's error below is joined with %v, not %w: nothing that error
// wraps must ever be reachable via errors.Is from SendKeys's return value,
// regardless of what the underlying backend's error happens to wrap.
var ErrKeysNotStarted = errors.New("agent: keys dispatch did not start")

// wrapNotStarted builds the error SendKeys returns for a pre-delivery ctx
// failure: wraps ErrKeysNotStarted (so errors.Is(result, ErrKeysNotStarted)
// is true) together with ctxErr's text for diagnostics, without wrapping
// ctxErr itself (so errors.Is(result, context.DeadlineExceeded) is false —
// callers must match by ErrKeysNotStarted's identity only, never by
// inspecting the underlying context error).
func wrapNotStarted(ctxErr error) error {
	return fmt.Errorf("%w: %v", ErrKeysNotStarted, ctxErr)
}

// tmuxOctalEscape encodes every byte of s as a three-digit octal escape
// ("\ddd"), for embedding inside a double-quoted tmux command-argument
// string. tmux's own command-argument parser reverses this encoding back to
// the exact original bytes before evaluating the argument, so the encoded
// form never writes any byte of s unescaped — including bytes that would
// otherwise need individual handling (quotes, backslashes, a trailing
// separator character, control bytes).
func tmuxOctalEscape(s string) string {
	const octalDigits = "01234567"
	b := make([]byte, 0, len(s)*4)
	for i := 0; i < len(s); i++ {
		c := s[i]
		b = append(b, '\\', octalDigits[(c>>6)&07], octalDigits[(c>>3)&07], octalDigits[c&07])
	}
	return string(b)
}

// sendKeysScript builds the single tmux command line SendKeys supplies on
// stdin to "tmux source-file -": one send-keys command addressing target,
// with keys embedded as a double-quoted, fully octal-escaped argument (see
// tmuxOctalEscape). tmux decodes the escapes back to the exact original
// bytes before send-keys applies its own named-key/literal-text
// interpretation, so the semantics — one string becomes one argument, a
// named key is recognized only on an exact whole-argument match, no
// automatic Enter — are unchanged from a direct send-keys invocation.
func sendKeysScript(target, keys string) string {
	return fmt.Sprintf("send-keys -t %s -- \"%s\"\n", target, tmuxOctalEscape(keys))
}

// minTmuxMajor/minTmuxMinor is the minimum tmux version the delivery
// mechanism requires (contract §2.3's "Transport requirements"): reading
// "source-file -" from stdin was added in tmux 3.1, one version after octal
// escapes inside a double-quoted command argument (tmuxOctalEscape's
// encoding) were added in 3.0 — so the 3.1 floor covers both.
const (
	minTmuxMajor = 3
	minTmuxMinor = 1
)

// tmuxVersionRe matches the numeric major/minor version tmux -V reports
// (e.g. "tmux 3.3a"), ignoring any trailing letter or suffix after the minor
// version.
var tmuxVersionRe = regexp.MustCompile(`^tmux (\d+)\.(\d+)`)

// parseTmuxVersion extracts the numeric major/minor version from tmux -V's
// output. It returns an error if out does not match tmux's own
// "tmux X.Y[suffix]" format.
func parseTmuxVersion(out string) (major, minor int, err error) {
	m := tmuxVersionRe.FindStringSubmatch(strings.TrimSpace(out))
	if m == nil {
		return 0, 0, fmt.Errorf("agent: could not parse tmux version from %q", out)
	}
	major, _ = strconv.Atoi(m[1])
	minor, _ = strconv.Atoi(m[2])
	return major, minor, nil
}

// tmuxVersionAtLeast reports whether major.minor meets or exceeds
// wantMajor.wantMinor.
func tmuxVersionAtLeast(major, minor, wantMajor, wantMinor int) bool {
	if major != wantMajor {
		return major > wantMajor
	}
	return minor >= wantMinor
}

// tmuxVersionCacheKey identifies one checkTmuxVersionSupported cache entry.
// Pairing ContainerID with Image and the "agent_id" label (rather than
// keying on ContainerID alone) matters because ContainerID is not always a
// fresh identifier assigned per container instance: on at least one backend
// it is a stable, reusable name (a Kubernetes pod name,
// pkg/runtime/k8s_runtime.go's AgentInfo.ContainerID assignment). Image
// covers a later container recreated under that same name with a different
// image; "agent_id" additionally covers a delete-and-recreate of an agent
// under the same slug and image (see resolveKeysTarget's identity check).
// This does not cover every case a reused ContainerID could produce — a
// same-slug, same-image, same-agent_id recreation with an unchanged (but
// now different) tmux binary is not distinguished by any of these fields —
// but that remaining case fails safe: an unrecognized too-old tmux simply
// fails at the delivery call itself, reported as an ordinary ambiguous
// error, never as a proven-before-delivery class and never replayed.
type tmuxVersionCacheKey struct {
	containerID string
	image       string
	agentID     string
}

// checkTmuxVersionSupported queries target's tmux version via "tmux -V" and
// reports whether it meets the delivery mechanism's minimum
// (minTmuxMajor/minTmuxMinor), returning ErrKeysUnsupported when it does
// not.
//
// It fails closed only on a version it can actually parse and determine to
// be below the floor. A query it cannot run, or output it cannot parse — a
// backend whose Exec cannot reach a shell that has tmux on its PATH, for
// example — is inconclusive, not proof of incompatibility, so it is treated
// as "cannot determine, do not block" rather than ErrKeysUnsupported: the
// readiness probe and the delivery call that follow remain the backstop for
// a genuinely incompatible or broken backend. A determined-supported result
// is cached by tmuxVersionCacheKey (see its doc comment for why ContainerID
// alone is not a safe key), since a given container instance's tmux binary
// does not change during its own lifetime; anything else — an inconclusive
// result, or a version proven below the floor — is never cached, so a
// transient Exec failure, or a genuinely too-old container, gets a fresh
// check on every call.
func (m *AgentManager) checkTmuxVersionSupported(ctx context.Context, target api.AgentInfo) error {
	key := tmuxVersionCacheKey{containerID: target.ContainerID, image: target.Image, agentID: target.Labels["agent_id"]}
	if v, ok := m.tmuxVersionOK.Load(key); ok && v.(bool) {
		return nil
	}
	out, err := m.Runtime.Exec(runtime.WithSensitiveExec(ctx), target.ContainerID, []string{"tmux", "-V"})
	if err != nil {
		return nil
	}
	major, minor, perr := parseTmuxVersion(out)
	if perr != nil {
		return nil
	}
	if !tmuxVersionAtLeast(major, minor, minTmuxMajor, minTmuxMinor) {
		return ErrKeysUnsupported
	}
	m.tmuxVersionOK.Store(key, true)
	return nil
}

// keysScope pins a SendKeys/SendKeysLocal call to exactly one identity
// dimension — Hub-linked project ID or local project-config directory path
// (never a project name) — matching whichever entry point built it.
// resolveKeysTarget uses whichever field is set as the List filter; see
// .design/agent-keys-contract.md §4.3 for the full invariant, including why
// the path dimension compares through projectkeys.ResolvedPathEqual.
type keysScope struct {
	projectID   string
	projectPath string
}

// filter returns the single label key/value this scope resolves containers
// by, for use as one entry in resolveKeysTarget's List filter.
func (s keysScope) filter() (key, value string) {
	if s.projectPath != "" {
		return projectkeys.LabelProjectPath, s.projectPath
	}
	return projectkeys.LabelProjectID, s.projectID
}

// empty reports whether neither identity dimension is set — the one case
// resolveKeysTarget's callers (SendKeys, SendKeysLocal) must reject before
// ever reaching List, rather than resolving against an unscoped filter.
func (s keysScope) empty() bool {
	return s.projectID == "" && s.projectPath == ""
}

// SendKeys sends the exact byte-for-byte keys string to an agent's tmux
// session, with no trailing Enter, no paste buffer and no debounce — the
// frozen primitive for the dedicated broker /keys route
// (.design/agent-keys-contract.md §4.3). Delivery is a single tmux command
// (see sendKeysScript) supplied on stdin to "tmux source-file -", rather
// than passed as a process argument. This requires tmux ≥ 3.1 (see
// minTmuxMajor/minTmuxMinor); checkTmuxVersionSupported gates delivery on
// that floor and returns ErrKeysUnsupported for a resolved container whose
// tmux is provably older.
//
// SendKeys performs the "agent_id" container-label
// identity check described in agentkeys.BrokerRequest's doc comment
// atomically with resolution: exactly one List-then-match resolves exactly
// one container (resolveKeysTarget), that container's own "agent_id" label
// is checked against expectedAgentID as part of that same resolution, and
// every subsequent step — acquiring the injection lock, the version check,
// and the readiness probe — acts on that same resolved container's ID.
// Immediately before delivery, SendKeys re-verifies target identity by
// resolving again and requiring the result still identifies the same target
// (sameTargetInstance) — a correctness hardening, not a relaxation of the
// binding above: a caller must still never check the label itself and then
// invoke a different, re-resolving primitive expecting SendKeys's own
// resolution to have been reused.
//
// projectID and expectedAgentID must both be non-empty: SendKeys fails
// closed to ErrTargetNotFound rather than falling back to an unscoped
// (project-blind) lookup, matching #2193's "reject unscoped target
// fallback" — callers (the runtimebroker handler) are expected to have
// already validated these are present, but SendKeys does not trust that and
// checks again itself.
//
// It serializes against the manager's existing message/interrupt injection
// critical section for the same resolved container (see injectionLock), so
// a keys call cannot interleave its tmux byte sequence with a concurrent
// buffered flush or interrupt delivery. Lock acquisition respects ctx's
// deadline: SendKeys returns an ErrKeysNotStarted-wrapped error (never one
// of the other sentinels below) without executing anything if ctx is done
// before the lock is acquired, and rechecks ctx again once the lock is held
// — before the version check, readiness probe and target re-verification
// run — and a second time immediately before the delivery call itself,
// after all three of those — covering a deadline that expires while waiting
// for the lock (the "control-channel semaphore/target-lock wait" the
// contract's execute-before enforcement names) — so a deadline lost during
// that wait can never still result in execution afterward. A readiness-probe
// failure that itself coincides with ctx expiry, and a ctx expiry
// discovered when the target re-verification's own resolution fails, are
// each still reported as ErrKeysNotStarted (see the probe's own comment,
// and the re-verification's), even though neither recheck above is
// directly adjacent to either of those two points.
// Callers arrange for ctx's deadline to reflect the Hub-issued
// execute-before timestamp (agentkeys.CapExecuteBefore) before calling
// SendKeys.
//
// It returns one of three agentkeys sentinels — ErrTargetNotFound,
// ErrAgentNotRunning, ErrTerminalNotReady — the package-local
// ErrKeysUnsupported (a backend that cannot deliver keys at all, or whose
// tmux is below the version floor above), or an error wrapping the
// package-local ErrKeysNotStarted (see that sentinel's doc comment for
// exactly which checks, proven to occur before delivery began, produce it),
// and only when it can prove the corresponding condition. Any other
// failure — including one where the delivery call itself may have partially
// run, and including one that happens to wrap a context error of its own —
// is a plain error that must never be mistaken for ErrKeysNotStarted;
// callers must not attempt to reclassify it as one of these sentinels by
// inspecting its wrapped errors — see ErrKeysNotStarted's doc comment,
// agentkeys.BrokerRequest's doc comment, and
// .design/agent-keys-contract.md §4.3.
//
// See SendKeysLocal for the additive local-scope sibling that shares this
// entire delivery core (sendKeysCore) under a different identity dimension.
func (m *AgentManager) SendKeys(ctx context.Context, projectID, agentSlug, expectedAgentID, keys string) error {
	// Reject malformed/oversized/empty keys before any resolution or Exec
	// attempt — contract §2.3 ("an empty string is rejected before
	// dispatch and never silently becomes Enter"). The runtimebroker
	// handler already enforces this before calling SendKeys, but SendKeys
	// does not trust that: a local-mode caller may invoke this primitive
	// directly under its own deadline, without going through that handler.
	if err := agentkeys.ValidateKeys(keys); err != nil {
		return err
	}
	if runtimeNamesWithoutKeysSupport[m.Runtime.Name()] {
		return ErrKeysUnsupported
	}
	scope := keysScope{projectID: projectID}
	if scope.empty() || expectedAgentID == "" {
		return agentkeys.ErrTargetNotFound
	}
	return m.sendKeysCore(ctx, scope, agentSlug, expectedAgentID, keys)
}

// SendKeysLocal is SendKeys's additive local-scope sibling (agent-raw design
// ruling, ptone/scion#2198/#2468 finding 3; see .design/agent-keys-contract.md
// §3/§4.3 for the full rationale and invariants): it serves a purely local
// project that was never linked to a Hub project, so its containers carry no
// "scion.project_id" label at all. It shares SendKeys's entire delivery core
// (sendKeysCore) unchanged — same atomic binding, lock, version/readiness
// checks, re-verification, deadline enforcement, no-replay, error
// classification — differing only in the scope dimension.
//
// projectPath must be the non-empty, already-resolved project-config
// directory from config.GetResolvedProjectDir (never a project name: two
// directories can share one). The caller resolves that scope and selects
// the single target agent within it before calling, exactly as for
// SendKeys; SendKeysLocal never falls back to an unlabeled or
// name-only match the way selectAgentTarget (Stop/Delete) does, and an
// empty projectPath or expectedAgentID fails closed to
// agentkeys.ErrTargetNotFound, the same as SendKeys. The two entry points'
// scopes are never mixed or retried into one another.
func (m *AgentManager) SendKeysLocal(ctx context.Context, projectPath, agentSlug, expectedAgentID, keys string) error {
	if err := agentkeys.ValidateKeys(keys); err != nil {
		return err
	}
	if runtimeNamesWithoutKeysSupport[m.Runtime.Name()] {
		return ErrKeysUnsupported
	}
	scope := keysScope{projectPath: projectPath}
	if scope.empty() || expectedAgentID == "" {
		return agentkeys.ErrTargetNotFound
	}
	return m.sendKeysCore(ctx, scope, agentSlug, expectedAgentID, keys)
}

// sendKeysCore is the shared delivery core behind both SendKeys and
// SendKeysLocal, identical in every step except which keysScope resolution
// is pinned to. See SendKeys's doc comment for the full set of guarantees
// (atomic identity binding, injection lock, version gate, readiness probe,
// pre-delivery re-verification, deadline enforcement at each checkpoint,
// sensitive-exec transport, and error classification) — all of it applies
// here unchanged, parameterized only by scope.
func (m *AgentManager) sendKeysCore(ctx context.Context, scope keysScope, agentSlug, expectedAgentID, keys string) error {
	target, err := m.resolveKeysTarget(ctx, scope, agentSlug, expectedAgentID)
	if err != nil {
		return err
	}

	lock := m.injectionLock(target.ContainerID)
	if err := lock.Lock(ctx); err != nil {
		return wrapNotStarted(err)
	}
	defer lock.Unlock()

	// The lock wait above may have consumed the entire remaining admission
	// window: recheck before doing any further work (contract §4.2,
	// "enforce expiration ... after control-channel semaphore/target-lock
	// waits").
	if err := ctx.Err(); err != nil {
		return wrapNotStarted(err)
	}

	// Version gate: a backend whose tmux predates the delivery mechanism's
	// minimum (contract §2.3's "Transport requirements") would otherwise pass
	// the readiness probe below and only fail at the delivery call itself,
	// which — being a genuine delivery attempt — cannot be reported as
	// anything but ambiguous. Checking first lets a provably-too-old tmux
	// fail closed as ErrKeysUnsupported instead.
	if err := m.checkTmuxVersionSupported(ctx, target); err != nil {
		return err
	}

	// Terminal readiness probe: a running container may not yet (or no
	// longer) have a live "scion" tmux session — e.g. between container
	// start and harness tmux initialization, or a session that exited. This
	// proves readiness before delivery runs, rather than after the fact:
	// delivery itself would fail the same way, but as a plain, ambiguous
	// error rather than the proven-before-execution ErrTerminalNotReady the
	// contract requires. Run against the same target.ContainerID the
	// original resolution proved, not a fresh lookup.
	probeCtx := runtime.WithSensitiveExec(ctx)
	if _, err := m.Runtime.Exec(probeCtx, target.ContainerID, []string{"tmux", "has-session", "-t", keysTarget}); err != nil {
		// The admission deadline may have fired during the probe itself,
		// rather than the session genuinely being unready — that is a
		// pre-delivery expiry (contract §2.5: "expired admission" →
		// keys_unavailable), not a proven-not-ready readiness verdict, so
		// it must not be reported as ErrTerminalNotReady.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return wrapNotStarted(ctxErr)
		}
		return agentkeys.ErrTerminalNotReady
	}

	// Re-verify target identity (a correctness hardening): re-resolve with
	// the same scope/agentSlug/expectedAgentID and require the result still
	// identifies the same target as the original resolution
	// (sameTargetInstance). Nothing here ever delivers to a target other
	// than the one originally resolved: a second resolveKeysTarget failure
	// returns its own sentinel or error unchanged (never reclassified), and
	// only an identity mismatch between the two resolutions itself produces
	// ErrTargetNotFound.
	revalidated, err := m.resolveKeysTarget(ctx, scope, agentSlug, expectedAgentID)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			// Nothing has been sent at this point: an expiry discovered via
			// this failure is exactly as "proven not to have started" as
			// one discovered directly, so it is worth the same honest
			// wrapNotStarted classification rather than whatever error
			// resolveKeysTarget's own List call happened to produce.
			return wrapNotStarted(ctxErr)
		}
		return err
	}
	if !sameTargetInstance(target, revalidated) {
		return agentkeys.ErrTargetNotFound
	}

	// Final check immediately before the delivery call (contract §4.2's
	// third enforcement point, "immediately before runtime execution").
	// Checked here, after the version check, readiness probe and
	// re-verification above, rather than relying on Exec's own ctx
	// handling, so an expiry detected at this instant is reported as
	// "proven not to have executed" rather than folded into whatever error
	// the delivery call itself would produce if it observed the same
	// cancellation mid-call.
	if err := ctx.Err(); err != nil {
		return wrapNotStarted(err)
	}

	sendCtx := runtime.WithSensitiveExec(ctx)
	script := sendKeysScript(keysTarget, keys)
	cmd := []string{"tmux", "source-file", "-"}
	if _, err := m.Runtime.ExecWithStdin(sendCtx, target.ContainerID, cmd, strings.NewReader(script)); err != nil {
		// Not %w, and not %v of err itself: once this call has been made, a
		// failure is ambiguous (the tmux command may have partially run),
		// never "proven not to have started" — see ErrKeysNotStarted's doc
		// comment for why nothing this error wraps may be reachable via
		// errors.Is from this return value, however the underlying backend
		// built it. Separately, err.Error() is never embedded here either:
		// today's backends only wrap a process's exit status into it, but
		// SendKeys must not rely on that — a backend whose error text ever
		// carried caller-supplied content (the keys payload, contract §5)
		// must not have it surface through this return value, which a
		// local-mode caller (cmd/keys.go) prints to the user. Only a fixed
		// message plus a sanitized, content-free error class is reported.
		return fmt.Errorf("failed to send keys to agent '%s': delivery failed (%s)", target.Name, sendKeysDeliveryErrorClass(err))
	}

	return nil
}

// sendKeysDeliveryErrorClass classifies a keys delivery failure into a
// fixed, content-free label for sendKeysCore's error message — never the
// error's own text, which may carry caller-supplied content. It
// distinguishes only the shapes useful for diagnostics without risking a
// leak: a context cancellation or deadline (the admission window elapsing
// mid-call), an *exec.ExitError's exit code (a process that ran and exited
// non-zero — the status only, never its output), or a generic fallback for
// anything else.
func sendKeysDeliveryErrorClass(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "context_deadline_exceeded"
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return fmt.Sprintf("exit_status_%d", exitErr.ExitCode())
	}
	return "delivery_failed"
}

// resolveKeysTarget resolves the single container sendKeysCore must act on
// and proves, before returning it, that: (a) it is the one and only
// container matching (scope, agentSlug); and (b) its "agent_id" label
// equals expectedAgentID. Callers must pass a non-empty scope (SendKeys and
// SendKeysLocal both guard this before calling in); resolveKeysTarget does
// not itself guard against an unscoped lookup. Any failure to prove one of
// these returns the matching agentkeys sentinel (never a slug-only or
// project-name-only fallback) — see SendKeys's doc comment for why no
// caller may split this resolution across two separate List calls.
func (m *AgentManager) resolveKeysTarget(ctx context.Context, scope keysScope, agentSlug, expectedAgentID string) (api.AgentInfo, error) {
	filterKey, filterValue := scope.filter()
	filter := map[string]string{
		"scion.name": strings.ToLower(agentSlug),
		filterKey:    filterValue,
	}
	agents, err := m.List(ctx, filter)
	if err != nil {
		return api.AgentInfo{}, fmt.Errorf("agentkeys: listing agents for %q: %w", agentSlug, err)
	}

	var matches []api.AgentInfo
	for _, a := range agents {
		if matchesAgentID(a, agentSlug) {
			matches = append(matches, a)
		}
	}
	matches = DedupeByContainerID(matches)

	if len(matches) != 1 {
		// Zero matches, or more than one distinct container matching the
		// same (slug, project) scope: fail closed rather than guess which
		// one to bind to (mirrors selectAgentTarget's ambiguity handling for
		// Stop/Delete). This also covers "wrong project": the filter above
		// already excludes containers labeled for a different project, so a
		// same-slug agent in another project never appears in agents at
		// all.
		return api.AgentInfo{}, agentkeys.ErrTargetNotFound
	}

	target := matches[0]
	gotID := target.Labels["agent_id"]
	if gotID == "" || gotID != expectedAgentID {
		// Fail closed on a missing/empty label (a container started outside
		// the Hub's own dispatch path, or before SCION_AGENT_ID injection
		// existed) or a mismatch (a same-slug agent recreated inside the
		// execute-before window) — never a slug-only match. See
		// agentkeys.BrokerRequest.AgentID's doc comment.
		return api.AgentInfo{}, agentkeys.ErrTargetNotFound
	}

	if target.Phase != string(state.PhaseRunning) {
		return api.AgentInfo{}, agentkeys.ErrAgentNotRunning
	}

	return target, nil
}

// sameTargetInstance reports whether b, from SendKeys's pre-delivery
// re-verification (see SendKeys's doc comment), still identifies the same
// target a's original resolution proved: ContainerID must match, and so
// must the per-instance identifier (currently only
// AgentInfo.Kubernetes.UID) whenever either side reports one. Comparing
// unconditionally on "either", not just "both", matters: a backend that
// reports a UID for one resolution and not the other is exactly the
// asymmetry an identity check must not treat as permissive, even though
// both calls resolving through the same List path makes that asymmetry
// unreachable today. ContainerID alone decides the comparison only when
// neither side reports a per-instance identifier at all.
func sameTargetInstance(a, b api.AgentInfo) bool {
	if a.ContainerID != b.ContainerID {
		return false
	}
	auid, buid := kubernetesUID(a), kubernetesUID(b)
	if auid != "" || buid != "" {
		return auid == buid
	}
	return true
}

// kubernetesUID returns a's Kubernetes pod UID, or "" if a reports none.
func kubernetesUID(a api.AgentInfo) string {
	if a.Kubernetes == nil {
		return ""
	}
	return a.Kubernetes.UID
}

// deliveryStepKind identifies how deliverImmediate must run a deliveryStep,
// replacing dispatch on a step's argv contents (e.g. cmd[1] == "load-buffer")
// with an explicit tag set once when the step is built.
type deliveryStepKind int

const (
	// stepSendKeys runs argv via Exec: an interrupt key, a bare Enter, or one
	// of the trailing confirmation Enters.
	stepSendKeys deliveryStepKind = iota
	// stepLoadBuffer runs argv via ExecWithStdin, streaming the message body.
	stepLoadBuffer
	// stepPasteBuffer runs argv via Exec. Its success marks the message as
	// delivered; its failure triggers best-effort buffer cleanup.
	stepPasteBuffer
)

// deliveryStep is one command in a deliverImmediate call.
type deliveryStep struct {
	kind deliveryStepKind
	argv []string
}

// deliverImmediate sends a message to an agent's tmux session right now,
// bypassing the message buffer. This is the low-level delivery mechanism
// used both for interrupt messages (called directly) and for buffered
// messages (called by the MessageBuffer when the debounce timer fires).
func (m *AgentManager) deliverImmediate(ctx context.Context, agentID, projectID string, message string, interrupt bool) error {
	// 1. Find the agent, scoped to project to prevent cross-project delivery
	filter := map[string]string{"scion.name": strings.ToLower(agentID)}
	if projectID != "" {
		filter["scion.project_id"] = projectID
	}
	agents, err := m.List(ctx, filter)
	if err != nil {
		return err
	}

	var agent *api.AgentInfo
	for _, a := range agents {
		if matchesAgentID(a, agentID) {
			agent = &a
			break
		}
	}

	if agent == nil {
		return errNoRunningContainer(agentID)
	}

	// Serialize against a concurrent SendKeys call (or another concurrent
	// deliverImmediate call — an interrupt racing a buffered flush) for the
	// same resolved container, so their tmux byte sequences cannot
	// interleave. See injectionLock's doc comment. This uses ctx as given:
	// interrupt messages carry the caller's own ctx, while buffered flushes
	// call in with context.Background() (NewManager's deliverFunc), which
	// never times out here — a hung tmux call inside a flush can hold this
	// lock indefinitely; flush's own bounded retry loop only runs after
	// deliverFunc (and therefore this whole call) has already returned, so
	// it does not bound this wait. See injectionLock's own doc comment for
	// why this is fail-closed rather than a deadlock.
	lock := m.injectionLock(agent.ContainerID)
	if err := lock.Lock(ctx); err != nil {
		return fmt.Errorf("failed to acquire injection lock for agent '%s': %w", agent.Name, err)
	}
	defer lock.Unlock()

	// 2. Resolve harness — probe both layouts (worktree vs shared-workspace
	// per .design/hub-shared-workspace-isolation.md) since the mode isn't
	// passed through this lookup path.
	harnessName := "generic"
	if agent.ProjectPath != "" {
		projectDir, _ := config.GetResolvedProjectDir(agent.ProjectPath)
		if projectDir == "" {
			projectDir = agent.ProjectPath
		}
		scionJSON := filepath.Join(config.ResolveAgentDir(projectDir, agent.Name), "scion-agent.json")
		if data, err := os.ReadFile(scionJSON); err == nil {
			var cfg api.ScionConfig
			if err := json.Unmarshal(data, &cfg); err == nil && cfg.Harness != "" {
				harnessName = cfg.Harness
			}
		}
	}
	h := harness.New(harnessName)

	// 3. Prepare commands
	var steps []deliveryStep

	if interrupt {
		if seq := h.GetInterruptSequence(); len(seq) > 0 {
			for _, key := range seq {
				steps = append(steps, deliveryStep{kind: stepSendKeys, argv: []string{"tmux", "send-keys", "-t", "scion:0", key}})
			}
		} else {
			key := h.GetInterruptKey()
			steps = append(steps, deliveryStep{kind: stepSendKeys, argv: []string{"tmux", "send-keys", "-t", "scion:0", key}})
		}
	}

	// bufName names the tmux buffer used below, if this delivery pastes a
	// message. It is computed once (nextMsgBufferName) and reused for both
	// the load and paste steps, and for cleanup if the paste step fails.
	var bufName string

	if message == "" {
		// Empty messages send a bare Enter keypress to trigger confirmations
		steps = append(steps, deliveryStep{kind: stepSendKeys, argv: []string{"tmux", "send-keys", "-t", "scion:0", "Enter"}})
	} else {
		// Use tmux paste buffer with bracketed paste (-p) instead of send-keys.
		// send-keys simulates typing character-by-character, which allows TUI
		// applications to intercept special characters as hotkeys (e.g., Gemini
		// CLI treats '!' as a shell-mode toggle). Bracketed paste wraps the
		// content in escape sequences (\e[200~...\e[201~) that signal the
		// application to treat all characters as literal pasted text.
		//
		// The message is loaded into a named buffer via stdin rather than
		// passed as a "tmux set-buffer" argv element: tmux's client-server
		// protocol caps a single command's argv at 16 KB, and a coalesced
		// batch of debounced messages can exceed that (ptone/scion#2256).
		// Streaming it over stdin has no such limit.
		//
		// The buffer name is unique per delivery (ptone/scion#2265): two
		// deliveries to the same agent are not otherwise serialised, and a
		// shared fixed name lets one delivery's paste consume the buffer
		// loaded for the other. "-d" removes the per-delivery buffer after a
		// successful paste; on paste failure it is deleted explicitly below,
		// since "-d" does not run when paste-buffer itself fails.
		bufName = nextMsgBufferName()
		steps = append(steps, deliveryStep{kind: stepLoadBuffer, argv: loadBufferArgv(bufName)})
		steps = append(steps, deliveryStep{kind: stepPasteBuffer, argv: pasteBufferArgv("scion:0", bufName)})
		steps = append(steps, deliveryStep{kind: stepSendKeys, argv: []string{"tmux", "send-keys", "-t", "scion:0", "Enter"}})
	}

	// 4. Execute. Once "tmux paste-buffer" succeeds, the message content is
	// already sitting in the agent's terminal input — a later failure (the
	// closing Enter, or one of the confirmation Enters below) must not
	// trigger a retry of the whole delivery, or the retried paste-buffer
	// would duplicate the already-visible text (ptone/scion#1866). Errors
	// from that point on are wrapped in PartialDeliveryError so the message
	// buffer's bounded retry knows not to retry them.
	delivered := false
	for _, step := range steps {
		var err error
		if step.kind == stepLoadBuffer {
			_, err = m.Runtime.ExecWithStdin(ctx, agent.ContainerID, step.argv, strings.NewReader(message))
		} else {
			_, err = m.Runtime.Exec(ctx, agent.ContainerID, step.argv)
		}
		if err != nil {
			if step.kind == stepPasteBuffer {
				// load-buffer succeeded (or this step wouldn't have run), but
				// the paste itself failed, so paste-buffer's own "-d" never
				// fired to clean up the per-delivery buffer named above.
				// Named buffers aren't evicted by buffer-limit, so without
				// this they would accumulate on the tmux server. Best-effort:
				// the failure is already being reported below, so a further
				// error here is ignored. Uses a ctx detached from the
				// caller's (context.WithoutCancel, with its own short
				// timeout) so the cleanup still runs when the caller's ctx
				// is already cancelled — which may be why paste-buffer
				// itself failed.
				cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				_, _ = m.Runtime.Exec(cleanupCtx, agent.ContainerID, []string{"tmux", "delete-buffer", "-b", bufName})
				cancel()
			}
			wrapped := fmt.Errorf("failed to send message to agent '%s': %w", agent.Name, err)
			if delivered {
				return &PartialDeliveryError{Err: wrapped}
			}
			return wrapped
		}
		if step.kind == stepPasteBuffer {
			delivered = true
		}
	}

	// After sending a message, send two extra Enter keypresses with a brief delay
	// to ensure the input is accepted by the agent. This runs only once the
	// message (if any) has already been pasted, so any failure here is
	// necessarily partial delivery too.
	if message != "" {
		enterCmd := []string{"tmux", "send-keys", "-t", "scion:0", "Enter"}
		for range 2 {
			select {
			case <-ctx.Done():
				return &PartialDeliveryError{Err: fmt.Errorf("context canceled before sending Enter to agent '%s': %w", agent.Name, ctx.Err())}
			case <-time.After(300 * time.Millisecond):
			}
			if _, err := m.Runtime.Exec(ctx, agent.ContainerID, enterCmd); err != nil {
				return &PartialDeliveryError{Err: fmt.Errorf("failed to send Enter to agent '%s': %w", agent.Name, err)}
			}
		}
	}

	return nil
}

func matchesAgentID(a api.AgentInfo, id string) bool {
	return a.Name == id || a.ContainerID == id || strings.TrimPrefix(a.Name, "/") == id
}
