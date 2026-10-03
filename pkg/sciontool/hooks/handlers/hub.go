/*
Copyright 2025 The Scion Authors.
*/

package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

	state "github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// HubHandler sends status updates to the Scion Hub.
type HubHandler struct {
	client *hub.Client

	// addresseeCachePath caches the assistant-reply addressee resolved by
	// creatorUserID; see readAddresseeCache.
	addresseeCachePath string
}

// NewHubHandler creates a new hub handler.
// Returns nil if the Hub client is not configured.
func NewHubHandler() *HubHandler {
	client := hub.NewClient()
	if client == nil || !client.IsConfigured() {
		return nil
	}
	home := os.Getenv("HOME")
	if home == "" {
		home = "/home/scion"
	}
	return &HubHandler{
		client:             client,
		addresseeCachePath: filepath.Join(home, addresseeCacheFile),
	}
}

// retryReserve is the part of the hook's budget a rate-limit retry must
// leave unspent: time for the retried send itself and for the status update
// that follows the mirror in Handle.
const retryReserve = time.Second

// forwardAssistantReply mirrors an assistant reply to the agent's creator as
// an outbound "assistant-reply" message. The hub requires an explicit
// addressee, so the reply is addressed to the creator by user ID; when the
// creator is unknown or is not a user (an agent created by another agent),
// the mirror is skipped. ctx is the hook's own budget (see Handle); a 429 is
// retried once if its Retry-After, plus retryReserve, fits in what remains.
// Best-effort: failures are logged, never returned.
func (h *HubHandler) forwardAssistantReply(ctx context.Context, text string, metadata map[string]string, thinkingFiltered bool) {
	creatorID, err := h.creatorUserID(ctx)
	if err != nil {
		log.Error("Hub: outbound assistant reply skipped, agent lookup failed: %v", err)
		return
	}
	if creatorID == "" {
		log.Debug("Hub: outbound assistant reply skipped: creator is unknown or not a user")
		return
	}

	msg := hub.OutboundMessage{
		RecipientID: creatorID,
		Msg:         text,
		Type:        "assistant-reply",
		Metadata:    metadata,
	}
	err = h.client.SendOutboundMessage(ctx, msg)
	if wait, ok := retryAfterWithinBudget(ctx, err); ok {
		log.Debug("Hub: outbound assistant reply rate limited, retrying in %s", wait)
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
			err = h.client.SendOutboundMessage(ctx, msg)
		case <-ctx.Done():
			timer.Stop()
			err = ctx.Err()
		}
	}
	if err != nil {
		log.Error("Hub: outbound assistant reply failed: %v", err)
		return
	}
	log.Debug("Hub: Forwarded assistant reply to message store (%d bytes, thinking_filtered=%v)",
		len(text), thinkingFiltered)
}

// creatorUserID returns the user ID of the agent's creator, or "" when the
// creator is unknown or is not a user. A user-created agent has
// CreatedBy == the user's ID and ancestry exactly [that user]; an agent
// created by another agent has the parent agent as CreatedBy and a longer
// ancestry chain. The first successful lookup is cached in the agent home
// and reused on later calls; lookup errors are returned uncached.
func (h *HubHandler) creatorUserID(ctx context.Context) (string, error) {
	agentID := h.client.AgentID()
	if cached, ok := readAddresseeCache(h.addresseeCachePath, agentID); ok {
		return cached.RecipientID, nil
	}
	self, err := h.client.GetSelf(ctx)
	if err != nil {
		// Not cached: a transient failure is retried on the next Stop.
		return "", err
	}
	recipientID := ""
	if self.CreatedBy != "" && len(self.Ancestry) == 1 && self.Ancestry[0] == self.CreatedBy {
		recipientID = self.CreatedBy
	}
	// createdBy and ancestry are immutable, so the decision (including a
	// skip) holds for the agent's lifetime.
	writeAddresseeCache(h.addresseeCachePath, addresseeCache{AgentID: agentID, RecipientID: recipientID})
	return recipientID, nil
}

// addresseeCacheFile is the agent-home file caching the assistant-reply
// addressee. An empty RecipientID records a decision to skip.
const addresseeCacheFile = ".scion-reply-addressee.json"

// addresseeCacheMaxBytes bounds the cache read.
const addresseeCacheMaxBytes = 4096

type addresseeCache struct {
	AgentID     string `json:"agentId"`
	RecipientID string `json:"recipientId"`
}

// readAddresseeCache returns the cached addressee for agentID. A missing,
// unreadable, corrupt or foreign-agent file reports ok=false so the caller
// resolves afresh.
func readAddresseeCache(path, agentID string) (addresseeCache, bool) {
	if path == "" || agentID == "" {
		return addresseeCache{}, false
	}
	data, err := dirfd.ReadFileNoFollow(path, addresseeCacheMaxBytes)
	if err != nil {
		return addresseeCache{}, false
	}
	var c addresseeCache
	if err := json.Unmarshal(data, &c); err != nil || c.AgentID != agentID {
		return addresseeCache{}, false
	}
	return c, true
}

// writeAddresseeCache atomically replaces the cache file. Failure only costs
// a lookup on the next Stop, so it is logged and otherwise ignored.
func writeAddresseeCache(path string, c addresseeCache) {
	if path == "" || c.AgentID == "" {
		return
	}
	data, err := json.Marshal(c)
	if err != nil {
		return
	}
	if err := dirfd.WriteFileNoFollow(path, data, 0600, 0, 0, dirfd.ReplaceLeaf); err != nil {
		log.Debug("Failed to cache assistant-reply addressee: %v", err)
	}
}

// retryAfterWithinBudget reports whether err is a 429 carrying a Retry-After
// that, plus retryReserve, elapses before ctx's deadline, and returns the
// wait.
func retryAfterWithinBudget(ctx context.Context, err error) (time.Duration, bool) {
	var statusErr *hub.HTTPStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusTooManyRequests || !statusErr.HasRetryAfter {
		return 0, false
	}
	if deadline, ok := ctx.Deadline(); ok && statusErr.RetryAfter+retryReserve > time.Until(deadline) {
		return 0, false
	}
	return statusErr.RetryAfter, true
}

// Handle processes an event and sends a status update to the Hub.
// It mirrors the sticky activity logic from StatusHandler: when the local activity
// is waiting_for_input or completed, non-new-work events won't overwrite it.
func (h *HubHandler) Handle(event *hooks.Event) error {
	if h == nil || h.client == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// callStart times the hub call this event triggers (if any), for
	// start-time attribution of hook-driven status updates. Observability
	// only: outcome and elapsed_ms, never the request body.
	callStart := time.Now()

	var err error
	switch event.Name {
	case hooks.EventSessionStart:
		// Session starting - report running phase with working activity (clears any sticky)
		log.Debug("Hub: Reporting running/working (session start)")
		err = h.client.ReportState(ctx, state.PhaseRunning, state.ActivityWorking, "Session started")

	case hooks.EventPromptSubmit, hooks.EventAgentStart:
		// New work events - always clear sticky status
		message := "Processing"
		if event.Data.Prompt != "" {
			message = truncateMessage(event.Data.Prompt, 100)
		}
		log.Debug("Hub: Reporting thinking")
		as := state.AgentState{Phase: state.PhaseRunning, Activity: state.ActivityThinking}
		err = h.client.UpdateStatus(ctx, hub.StatusUpdate{
			Activity: state.ActivityThinking,
			Status:   as.DisplayStatus(),
			Message:  message,
		})

	case hooks.EventModelStart:
		// Model start - report thinking, but respect sticky activity
		if h.isLocalActivitySticky() {
			log.Debug("Hub: Skipping thinking (local activity is sticky)")
			return nil
		}
		message := "Processing"
		if event.Data.Prompt != "" {
			message = truncateMessage(event.Data.Prompt, 100)
		}
		log.Debug("Hub: Reporting thinking (model-start)")
		as := state.AgentState{Phase: state.PhaseRunning, Activity: state.ActivityThinking}
		err = h.client.UpdateStatus(ctx, hub.StatusUpdate{
			Activity: state.ActivityThinking,
			Status:   as.DisplayStatus(),
			Message:  message,
		})

	case hooks.EventToolStart:
		// Claude-specific: ExitPlanMode and AskUserQuestion mean waiting for user
		if event.Dialect == "claude" && (event.Data.ToolName == "ExitPlanMode" || event.Data.ToolName == "AskUserQuestion") {
			message := "Waiting for input"
			if event.Data.ToolName == "ExitPlanMode" {
				message = "Waiting for plan approval"
			}
			log.Debug("Hub: Reporting waiting_for_input (waiting: %s)", event.Data.ToolName)
			as := state.AgentState{Phase: state.PhaseRunning, Activity: state.ActivityWaitingForInput}
			err = h.client.UpdateStatus(ctx, hub.StatusUpdate{
				Activity: state.ActivityWaitingForInput,
				Status:   as.DisplayStatus(),
				Message:  message,
			})
			break
		}

		// Tool-start clears waiting_for_input (user has responded) but
		// preserves completed (tools may fire after task_completed as wrap-up).
		localActivity := readLocalActivity()
		if localActivity == string(state.ActivityCompleted) || localActivity == string(state.ActivityLimitsExceeded) {
			log.Debug("Hub: Skipping executing (completed is sticky, post-completion tool)")
			return nil
		}

		// Agent is executing a tool
		message := "Executing tool"
		if event.Data.ToolName != "" {
			message = "Executing: " + event.Data.ToolName
		}
		log.Debug("Hub: Reporting executing (tool: %s)", event.Data.ToolName)
		as := state.AgentState{Phase: state.PhaseRunning, Activity: state.ActivityExecuting}
		err = h.client.UpdateStatus(ctx, hub.StatusUpdate{
			Activity: state.ActivityExecuting,
			ToolName: event.Data.ToolName,
			Status:   as.DisplayStatus(),
			Message:  message,
		})

	case hooks.EventToolEnd, hooks.EventAgentEnd, hooks.EventModelEnd:
		// Forward assistant text (when the dialect extracted it — e.g.
		// Claude's Stop hook via transcript_path) to the hub message
		// store as an outbound agent→user reply. This is what makes
		// assistant responses show up in the Messages tab. Best-effort:
		// failure here must not break the status update flow below.
		//
		// Content-type filtering: AssistantText is pre-filtered by the
		// dialect layer (thinking/reasoning blocks stripped).
		if event.Name == hooks.EventAgentEnd && event.Data.AssistantText != "" {
			text := truncateAssistantText(event.Data.AssistantText)

			// Build metadata tags for content classification.
			metadata := map[string]string{
				"source": "hook",
			}
			if event.Data.AssistantContent != nil && event.Data.AssistantContent.HasThinking() {
				metadata["has_thinking"] = "true"
			}

			h.forwardAssistantReply(ctx, text, metadata,
				event.Data.AssistantContent != nil && event.Data.AssistantContent.HasThinking())
		}

		// Check if local activity is sticky before sending working
		if h.isLocalActivitySticky() {
			log.Debug("Hub: Skipping working (local activity is sticky)")
			return nil
		}
		log.Debug("Hub: Reporting working (step completed)")
		as := state.AgentState{Phase: state.PhaseRunning, Activity: state.ActivityWorking}
		err = h.client.UpdateStatus(ctx, hub.StatusUpdate{
			Activity: state.ActivityWorking,
			Status:   as.DisplayStatus(),
			Message:  "Ready",
		})

	case hooks.EventNotification:
		// Agent is waiting for input
		message := "Waiting for input"
		if event.Data.Message != "" {
			message = truncateMessage(event.Data.Message, 100)
		}
		log.Debug("Hub: Reporting waiting_for_input (notification)")
		as := state.AgentState{Phase: state.PhaseRunning, Activity: state.ActivityWaitingForInput}
		err = h.client.UpdateStatus(ctx, hub.StatusUpdate{
			Activity: state.ActivityWaitingForInput,
			Status:   as.DisplayStatus(),
			Message:  message,
		})

	case hooks.EventResponseComplete:
		summary := "Task completed"
		if event.Data.Message != "" {
			summary = truncateMessage(event.Data.Message, 200)
		}
		log.Debug("Hub: Reporting task completed (response-complete)")
		as := state.AgentState{Phase: state.PhaseRunning, Activity: state.ActivityCompleted}
		err = h.client.UpdateStatus(ctx, hub.StatusUpdate{
			Activity:    state.ActivityCompleted,
			Status:      as.DisplayStatus(),
			TaskSummary: summary,
		})

	case hooks.EventSessionEnd:
		// Session ended
		log.Debug("Hub: Reporting stopped (session end)")
		as := state.AgentState{Phase: state.PhaseStopped}
		err = h.client.UpdateStatus(ctx, hub.StatusUpdate{
			Phase:   state.PhaseStopped,
			Status:  as.DisplayStatus(),
			Message: "Session ended",
		})

	default:
		// No status update for this event
		return nil
	}

	elapsedMs := time.Since(callStart).Milliseconds()
	if err != nil {
		// One structured line, not two: slog.Default() routes through this
		// package's own log handler (see pkg/sciontool/log), so this already
		// reaches agent.log and stderr exactly like the former log.Error call
		// did — adding a second line here would double-log every failure,
		// and hook events can fire once per tool call while the hub is down.
		// Don't return error - we don't want Hub failures to break the hook chain
		slog.Error("hub status update failed", "event", event.Name, "elapsed_ms", elapsedMs, "error", err)
	} else {
		log.Debug("Hub status update sent successfully")
		// SessionStart is the one event currently used for start-time
		// attribution (see pkg/hub's since_create_ms), so it logs at Info;
		// every other event logs the same shape at Debug.
		if event.Name == hooks.EventSessionStart {
			slog.Info("hub status update sent", "event", event.Name, "elapsed_ms", elapsedMs)
		} else {
			slog.Debug("hub status update sent", "event", event.Name, "elapsed_ms", elapsedMs)
		}
	}

	return nil
}

// isLocalActivitySticky reads the local agent-info.json (written by StatusHandler
// which runs before HubHandler) and returns true if the activity is sticky.
func (h *HubHandler) isLocalActivitySticky() bool {
	activity := readLocalActivity()
	return isStickyActivity(activity)
}

// readLocalActivity reads the current activity from the local agent-info.json file.
func readLocalActivity() string {
	home := os.Getenv("HOME")
	if home == "" {
		home = "/home/scion"
	}
	statusPath := filepath.Join(home, "agent-info.json")

	data, err := os.ReadFile(statusPath)
	if err != nil {
		return ""
	}

	var info map[string]interface{}
	if err := json.Unmarshal(data, &info); err != nil {
		return ""
	}

	activity, _ := info["activity"].(string)
	return activity
}

// ReportWaitingForInput sends a waiting-for-input status to the Hub.
func (h *HubHandler) ReportWaitingForInput(message string) error {
	if h == nil || h.client == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	log.Debug("Hub: Reporting waiting_for_input (ask_user: %s)", truncateMessage(message, 50))
	as := state.AgentState{Phase: state.PhaseRunning, Activity: state.ActivityWaitingForInput}
	return h.client.UpdateStatus(ctx, hub.StatusUpdate{
		Activity: state.ActivityWaitingForInput,
		Status:   as.DisplayStatus(),
		Message:  message,
	})
}

// ReportTaskCompleted sends a task-completed status to the Hub.
func (h *HubHandler) ReportTaskCompleted(taskSummary string) error {
	if h == nil || h.client == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	log.Debug("Hub: Reporting task completed: %s", truncateMessage(taskSummary, 50))
	as := state.AgentState{Phase: state.PhaseRunning, Activity: state.ActivityCompleted}
	return h.client.UpdateStatus(ctx, hub.StatusUpdate{
		Activity:    state.ActivityCompleted,
		Status:      as.DisplayStatus(),
		TaskSummary: taskSummary,
	})
}

// ReportLimitsExceeded sends a limits-exceeded status to the Hub.
func (h *HubHandler) ReportLimitsExceeded(message string) error {
	if h == nil || h.client == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	log.Debug("Hub: Reporting limits_exceeded: %s", truncateMessage(message, 50))
	as := state.AgentState{Phase: state.PhaseRunning, Activity: state.ActivityLimitsExceeded}
	return h.client.UpdateStatus(ctx, hub.StatusUpdate{
		Activity: state.ActivityLimitsExceeded,
		Status:   as.DisplayStatus(),
		Message:  message,
	})
}

// ReportCounts sends current turn and model call counts to the Hub.
func (h *HubHandler) ReportCounts(turnCount, modelCallCount int) error {
	if h == nil || h.client == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	log.Debug("Hub: Reporting counts (turns=%d, model_calls=%d)", turnCount, modelCallCount)
	return h.client.UpdateStatus(ctx, hub.StatusUpdate{
		CurrentTurns:      &turnCount,
		CurrentModelCalls: &modelCallCount,
	})
}

// truncateAssistantText caps an assistant reply at the hub's message-length
// limit, measured in runes as the hub measures it, keeping the start of the
// reply and reserving room for a marker reporting how many were dropped.
func truncateAssistantText(text string) string {
	total := utf8.RuneCountInString(text)
	if total <= messages.MaxMessageLength {
		return text
	}

	marker := func(dropped int) string {
		return fmt.Sprintf("\n[truncated, %d characters omitted]", dropped)
	}
	// Sized against the worst case: the dropped count can only shrink the marker.
	keep := messages.MaxMessageLength - utf8.RuneCountInString(marker(total))
	if keep <= 0 {
		// Defensive: unreachable at the current limit, but a negative slice
		// would panic and no caller up to dispatchEvent recovers.
		return string([]rune(text)[:messages.MaxMessageLength])
	}
	return string([]rune(text)[:keep]) + marker(total-keep)
}

// truncateMessage truncates a message to the specified length.
func truncateMessage(msg string, maxLen int) string {
	if len(msg) <= maxLen {
		return msg
	}
	return msg[:maxLen-3] + "..."
}
