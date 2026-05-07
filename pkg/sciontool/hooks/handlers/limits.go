/*
Copyright 2025 The Scion Authors.
*/

package handlers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	state "github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// ExitCodeLimitsExceeded is the exit code used when an agent is stopped due to
// exceeding configured limits (max_turns, max_model_calls, or max_duration).
const ExitCodeLimitsExceeded = 10

// LimitsState represents the persisted limit counters in agent-limits.json.
type LimitsState struct {
	TurnCount      int    `json:"turn_count"`
	ModelCallCount int    `json:"model_call_count"`
	MaxTurns       int    `json:"max_turns"`
	MaxModelCalls  int    `json:"max_model_calls"`
	StartedAt      string `json:"started_at"`
}

// LimitsHandler tracks turn and model call counts and enforces configured limits.
// When a limit is exceeded, it updates the agent status, logs the event, reports
// to the Hub, and sends SIGUSR1 to PID 1 (sciontool init) to initiate shutdown.
type LimitsHandler struct {
	maxTurns        int
	maxModelCalls   int
	limitsPath      string
	triggerFilePath string
	statusHandler   *StatusHandler
}

// NewLimitsHandler creates a new limits handler.
// Reads SCION_MAX_TURNS and SCION_MAX_MODEL_CALLS from the environment.
// Returns nil if no limits are configured.
func NewLimitsHandler() *LimitsHandler {
	maxTurns := ParseEnvInt("SCION_MAX_TURNS")
	maxModelCalls := ParseEnvInt("SCION_MAX_MODEL_CALLS")

	if maxTurns <= 0 && maxModelCalls <= 0 {
		return nil
	}

	home := os.Getenv("HOME")
	if home == "" {
		home = "/home/scion"
	}

	return &LimitsHandler{
		maxTurns:        maxTurns,
		maxModelCalls:   maxModelCalls,
		limitsPath:      filepath.Join(home, "agent-limits.json"),
		triggerFilePath: LimitsTriggerFile,
		statusHandler:   NewStatusHandler(),
	}
}

// NewLimitsHandlerWithPath creates a LimitsHandler with an explicit path (for testing).
func NewLimitsHandlerWithPath(maxTurns, maxModelCalls int, limitsPath string) *LimitsHandler {
	if maxTurns <= 0 && maxModelCalls <= 0 {
		return nil
	}
	return &LimitsHandler{
		maxTurns:        maxTurns,
		maxModelCalls:   maxModelCalls,
		limitsPath:      limitsPath,
		triggerFilePath: LimitsTriggerFile,
		statusHandler:   NewStatusHandler(),
	}
}

// Handle processes a hook event and increments the appropriate counter.
// On agent-end: increments turn count, checks max_turns.
// On model-end: increments model call count, checks max_model_calls.
func (h *LimitsHandler) Handle(event *hooks.Event) error {
	if h == nil {
		return nil
	}

	switch event.Name {
	case hooks.EventAgentEnd:
		if h.maxTurns <= 0 {
			return nil
		}
		return h.incrementAndCheck("turn_count", h.maxTurns, "max_turns")

	case hooks.EventModelEnd:
		if h.maxModelCalls <= 0 {
			return nil
		}
		return h.incrementAndCheck("model_call_count", h.maxModelCalls, "max_model_calls")

	default:
		return nil
	}
}

// InitLimitsFile creates or resets the agent-limits.json file.
// Called during post-start to initialize counters (they reset on each start/resume).
func InitLimitsFile(limitsPath string, maxTurns, maxModelCalls int) error {
	ls := LimitsState{
		TurnCount:      0,
		ModelCallCount: 0,
		MaxTurns:       maxTurns,
		MaxModelCalls:  maxModelCalls,
		StartedAt:      time.Now().UTC().Format(time.RFC3339),
	}
	return writeLimitsState(limitsPath, &ls)
}

// incrementAndCheck reads the limits file, increments the given counter field,
// checks if the limit is exceeded, and triggers shutdown if so.
func (h *LimitsHandler) incrementAndCheck(counterField string, limit int, limitName string) error {
	ls, err := h.readLimitsState()
	if err != nil {
		// If we can't read the file, log and continue - don't crash the hook pipeline
		log.Error("Failed to read agent-limits.json: %v", err)
		return nil
	}

	// Increment the appropriate counter
	var count int
	switch counterField {
	case "turn_count":
		ls.TurnCount++
		count = ls.TurnCount
	case "model_call_count":
		ls.ModelCallCount++
		count = ls.ModelCallCount
	}

	// Write the updated state
	if err := writeLimitsState(h.limitsPath, ls); err != nil {
		log.Error("Failed to write agent-limits.json: %v", err)
		return nil
	}

	// Report updated counts to Hub
	hubHandler := NewHubHandler()
	if hubHandler != nil {
		if err := hubHandler.ReportCounts(ls.TurnCount, ls.ModelCallCount); err != nil {
			log.Error("Failed to report counts to Hub: %v", err)
		}
	}

	// Check if the limit is exceeded
	if count >= limit {
		message := fmt.Sprintf("%s of %d exceeded (completed %d)", limitName, limit, count)
		h.triggerLimitsExceeded(message)
	}

	return nil
}

// triggerLimitsExceeded updates status, logs the event, and signals PID 1.
func (h *LimitsHandler) triggerLimitsExceeded(message string) {
	// 1. Update agent-info.json to LIMITS_EXCEEDED (sticky)
	if err := h.statusHandler.UpdateActivity(state.ActivityLimitsExceeded, ""); err != nil {
		log.Error("Failed to set limits_exceeded status: %v", err)
	}

	// 2. Log the event
	log.TaggedInfo("LIMITS_EXCEEDED", "Agent stopped: %s", message)

	// 3. Report to Hub if configured
	hubHandler := NewHubHandler()
	if hubHandler != nil {
		if err := hubHandler.ReportLimitsExceeded(message); err != nil {
			log.Error("Failed to report limits_exceeded to Hub: %v", err)
		}
	}

	// 4. Signal init process to initiate shutdown (trigger file + SIGUSR1 fallback)
	if err := h.signalLimitsExceeded(); err != nil {
		log.Error("Failed to signal limits exceeded: %v", err)
	}
}

// readLimitsState reads the agent-limits.json file.
func (h *LimitsHandler) readLimitsState() (*LimitsState, error) {
	data, err := os.ReadFile(h.limitsPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", h.limitsPath, err)
	}
	var ls LimitsState
	if err := json.Unmarshal(data, &ls); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", h.limitsPath, err)
	}
	return &ls, nil
}

// writeLimitsState writes the limits state to disk atomically.
func writeLimitsState(path string, ls *LimitsState) error {
	data, err := json.MarshalIndent(ls, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling limits state: %w", err)
	}

	dir := filepath.Dir(path)
	tmpFile, err := os.CreateTemp(dir, "agent-limits-*.json")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("writing temp file: %w", err)
	}
	tmpFile.Close()

	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("atomic rename: %w", err)
	}

	return nil
}

// LimitsTriggerFile is the well-known path for the limits-exceeded trigger file.
// When a hook handler detects a limit is exceeded, it creates this file.
// The init process watches for it to initiate shutdown.
const LimitsTriggerFile = "/tmp/scion-limits-exceeded"

// signalLimitsExceeded notifies PID 1 that a limit has been exceeded.
// It writes a trigger file and also attempts SIGUSR1 as a fallback.
func (h *LimitsHandler) signalLimitsExceeded() error {
	triggerPath := h.triggerFilePath
	if triggerPath == "" {
		triggerPath = LimitsTriggerFile
	}
	// Primary mechanism: create a trigger file that init watches for.
	// This works regardless of UID differences between the hook process
	// and PID 1 (init runs as root, hooks run as the scion user).
	if err := os.WriteFile(triggerPath, []byte("exceeded"), 0666); err != nil {
		log.Error("Failed to write limits trigger file: %v", err)
	}

	// Fallback: send SIGUSR1 to PID 1. This may fail with EPERM when the
	// hook process runs as a non-root user and PID 1 runs as root.
	p, err := os.FindProcess(1)
	if err != nil {
		return fmt.Errorf("finding PID 1: %w", err)
	}
	if err := p.Signal(syscall.SIGUSR1); err != nil {
		// Expected to fail when running as non-root; the trigger file
		// is the reliable mechanism.
		log.Debug("SIGUSR1 to PID 1 failed (expected if non-root): %v", err)
		return nil
	}
	return nil
}

// ParseEnvInt reads an integer from an environment variable. Returns 0 if unset or invalid.
func ParseEnvInt(key string) int {
	val := os.Getenv(key)
	if val == "" {
		return 0
	}
	n, err := strconv.Atoi(val)
	if err != nil {
		return 0
	}
	return n
}
