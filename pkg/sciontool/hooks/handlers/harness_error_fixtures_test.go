/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/dialects"
	"github.com/GoogleCloudPlatform/scion/pkg/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests in this file replay real hook payloads captured from each
// harness CLI on a turn whose model call got HTTP 400 (or 401) from a local
// mock endpoint (see the README next to each fixture under
// pkg/sciontool/hooks/dialects/testdata). Each record goes through the
// dialect the harness uses at runtime, then the StatusHandler and the
// HubHandler in hook order, against an httptest Hub (hubRecorderSetup).

// errorFixtureStep is the result of replaying one fixture record.
type errorFixtureStep struct {
	event    *hooks.Event
	activity string // local agent-info.json activity after the record
	message  string // local detail.message after the record
	reports  []hubReport
}

// errorFixturePath returns the path of a fixture under
// pkg/sciontool/hooks/dialects/testdata.
func errorFixturePath(t *testing.T, harness, name string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	return filepath.Join(filepath.Dir(file), "..", "dialects", "testdata", harness, name)
}

// loadErrorFixture reads every JSON record of a fixture, in capture order.
func loadErrorFixture(t *testing.T, harness, name string) []map[string]interface{} {
	t.Helper()
	f, err := os.Open(errorFixturePath(t, harness, name))
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	var records []map[string]interface{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if scanner.Text() == "" {
			continue
		}
		var rec map[string]interface{}
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &rec))
		records = append(records, rec)
	}
	require.NoError(t, scanner.Err())
	require.NotEmpty(t, records)
	return records
}

// mappingDialect loads harnesses/<harness>/dialect.yaml, the dialect that
// wins over a built-in one at runtime.
func mappingDialect(t *testing.T, harness string) hooks.Dialect {
	t.Helper()
	md, err := dialects.LoadMappingDialect(testutil.HarnessDialectPath(t, harness))
	require.NoError(t, err)
	return md
}

// replayErrorFixture replays records through d, the StatusHandler and the
// HubHandler, starting from a ready agent (working, "Harness started").
// session-end records are skipped: they stop the agent, which says
// nothing about the turn.
func replayErrorFixture(t *testing.T, d hooks.Dialect, records []map[string]interface{}) []errorFixtureStep {
	t.Helper()
	sh, hh, reports := hubRecorderSetup(t)
	writeAgentInfoJSON(t, sh, `{"phase":"running","activity":"working","detail":{"message":"Harness started"}}`)

	var steps []errorFixtureStep
	for _, rec := range records {
		event, err := d.Parse(rec)
		require.NoError(t, err)
		if event.Name == hooks.EventSessionEnd {
			continue
		}
		before := len(reports())
		require.NoError(t, sh.Handle(event))
		require.NoError(t, hh.Handle(event))
		info := readAgentInfoMap(t, sh.StatusPath)
		activity, _ := info["activity"].(string)
		steps = append(steps, errorFixtureStep{
			event:    event,
			activity: activity,
			message:  detailMessage(info),
			reports:  reports()[before:],
		})
	}
	return steps
}

// eventNames returns the normalized event names of steps.
func eventNames(steps []errorFixtureStep) []string {
	names := make([]string, len(steps))
	for i, s := range steps {
		names[i] = s.event.Name
	}
	return names
}

// assertHarnessErrorStep checks that step set harness_error with reason,
// locally and on the Hub.
func assertHarnessErrorStep(t *testing.T, step errorFixtureStep, reason string) {
	t.Helper()
	assert.Equal(t, "harness_error", step.activity)
	assert.Equal(t, reason, step.message)
	require.Len(t, step.reports, 1)
	assert.Equal(t, hubReport{Activity: "harness_error", Status: "harness_error", Message: reason}, step.reports[0])
}

// assertClearedStep checks that step replaced harness_error with activity
// and dropped the reason, locally and on the Hub.
func assertClearedStep(t *testing.T, step errorFixtureStep, activity string) {
	t.Helper()
	assert.Equal(t, activity, step.activity)
	assert.Empty(t, step.message)
	require.Len(t, step.reports, 1)
	assert.Equal(t, activity, step.reports[0].Activity)
}

const fixtureProviderText = "Permission 'aiplatform.endpoints.predict' denied on resource (or it may not exist)."

// TestClaudeFixture_StopFailureSetsHarnessError covers Claude Code 2.1.325 in
// its TUI: a 400 turn fires StopFailure (no Stop), which sets harness_error
// with the provider text; the next prompt clears it (criterion 14) and its
// failure sets it again.
func TestClaudeFixture_StopFailureSetsHarnessError(t *testing.T) {
	steps := replayErrorFixture(t, dialects.NewClaudeDialect(), loadErrorFixture(t, "claude", "api-error-400-2.1.325.jsonl"))
	require.Equal(t, []string{
		hooks.EventSessionStart, hooks.EventPromptSubmit, hooks.EventAgentError,
		hooks.EventPromptSubmit, hooks.EventAgentError,
	}, eventNames(steps))

	reason := "model API: API Error: 400 " + fixtureProviderText
	assertHarnessErrorStep(t, steps[2], reason)
	assertClearedStep(t, steps[3], "thinking")
	assertHarnessErrorStep(t, steps[4], reason)
}

// TestClaudeFixture_StopFailureAuthCategory covers a 401: StopFailure's
// error category is kept in front of the provider text.
func TestClaudeFixture_StopFailureAuthCategory(t *testing.T) {
	steps := replayErrorFixture(t, dialects.NewClaudeDialect(), loadErrorFixture(t, "claude", "api-error-401-2.1.325.jsonl"))
	require.Equal(t, []string{hooks.EventSessionStart, hooks.EventPromptSubmit, hooks.EventAgentError}, eventNames(steps))
	assertHarnessErrorStep(t, steps[2],
		"model API: authentication_failed: Please run /login · API Error: 401 "+fixtureProviderText)
}

// TestAntigravityFixture_StopErrorSetsHarnessError covers agy 1.5.9: the
// 400 turn is PreInvocation then Stop with error and no PostInvocation, so
// no error-free model-end can arrive after Stop and clear harness_error.
// The next turn's PreInvocation clears it (criterion 14).
func TestAntigravityFixture_StopErrorSetsHarnessError(t *testing.T) {
	steps := replayErrorFixture(t, mappingDialect(t, "antigravity"), loadErrorFixture(t, "antigravity", "api-error-400-1.5.9.jsonl"))
	require.Equal(t, []string{
		hooks.EventModelStart, hooks.EventAgentEnd,
		hooks.EventModelStart, hooks.EventAgentEnd,
	}, eventNames(steps))

	reason := "model API: generating and executing: Error 400, Message: " + fixtureProviderText +
		", Status: FAILED_PRECONDITION, Details: []"
	assertHarnessErrorStep(t, steps[1], reason)
	assertClearedStep(t, steps[2], "thinking")
	assertHarnessErrorStep(t, steps[3], reason)
}

// TestGrokFixture_StopFailureSetsHarnessError covers grok 1.0.77: the
// Notification it sends first (notificationType agent_error) is ignored
// (dialect skip_when), so it sets no waiting_for_input and sends no
// input-needed alert with the provider text; the StopFailure that follows,
// whose errorDetails carry the status and provider text, sets
// harness_error. The next prompt clears it. The capture ended by killing
// grok, which sent SessionEnd and then a Stop with reason shutdown; that
// Stop is ignored too (it ends no turn and would report working after
// session-end).
func TestGrokFixture_StopFailureSetsHarnessError(t *testing.T) {
	steps := replayErrorFixture(t, mappingDialect(t, "grok-build"), loadErrorFixture(t, "grok-build", "api-error-400-1.0.77.jsonl"))
	require.Equal(t, []string{
		hooks.EventSessionStart, hooks.EventPromptSubmit, hooks.EventIgnored, hooks.EventAgentError,
		hooks.EventPromptSubmit, hooks.EventIgnored, hooks.EventAgentError,
		hooks.EventIgnored,
	}, eventNames(steps))

	for _, i := range []int{2, 5} {
		assert.Equal(t, "thinking", steps[i].activity)
		assert.Empty(t, steps[i].reports)
	}
	assertNoWaitingForInput(t, append(append([]errorFixtureStep{}, steps[:3]...), steps[4:6]...), fixtureProviderText)

	reason := "model API: API error (status 400 Bad Request): invalid_request_error: " + fixtureProviderText
	assertHarnessErrorStep(t, steps[3], reason)
	assertClearedStep(t, steps[4], "thinking")
	assertHarnessErrorStep(t, steps[6], reason)
	assert.Equal(t, "harness_error", steps[7].activity)
	assert.Empty(t, steps[7].reports)
}

// TestPiFixture_AgentEndErrorSetsHarnessError covers the pi bridge's
// agent_end with error (testdata/pi/bridge-payloads.jsonl).
func TestPiFixture_AgentEndErrorSetsHarnessError(t *testing.T) {
	var records []map[string]interface{}
	for _, rec := range loadErrorFixture(t, "pi", "bridge-payloads.jsonl") {
		if rec["hook_event_name"] == "agent_end" && rec["error"] != nil {
			records = append(records, rec)
		}
	}
	require.Len(t, records, 1)

	steps := replayErrorFixture(t, mappingDialect(t, "pi"), records)
	require.Len(t, steps, 1)
	assertHarnessErrorStep(t, steps[0], "model API: 429 rate limited: retry later")
}

// TestCodexFixture_FailedTurnIsNotCompleted covers codex 0.169.0 in its
// TUI (criterion 15): a 400 turn fires no Stop and no notify, so nothing
// ends the turn, and it never becomes completed. The hook path has no
// error to report; the agent stays thinking.
func TestCodexFixture_FailedTurnIsNotCompleted(t *testing.T) {
	steps := replayErrorFixture(t, mappingDialect(t, "codex"), loadErrorFixture(t, "codex", "api-error-400-0.169.0.jsonl"))
	require.Equal(t, []string{hooks.EventSessionStart, hooks.EventPromptSubmit}, eventNames(steps))
	last := steps[len(steps)-1]
	assert.Equal(t, "thinking", last.activity)
	for _, s := range steps {
		assert.NotEqual(t, "completed", s.activity)
		for _, r := range s.reports {
			assert.NotEqual(t, "completed", r.Activity)
		}
	}
}

// TestGeminiFixture_AfterAgentCarriesNoError records the Gemini CLI 0.68.0
// gap: the 400 turn's AfterAgent has no error field, so the hook path
// cannot tell it failed and ends the turn in working. The OTel API-error
// watchdog covers Gemini CLI instead.
func TestGeminiFixture_AfterAgentCarriesNoError(t *testing.T) {
	steps := replayErrorFixture(t, dialects.NewGeminiDialect(), loadErrorFixture(t, "gemini-cli", "api-error-400-0.68.0.jsonl"))
	require.Equal(t, []string{
		hooks.EventSessionStart, hooks.EventAgentStart, hooks.EventModelStart, hooks.EventAgentEnd,
	}, eventNames(steps))
	last := steps[len(steps)-1]
	assert.Empty(t, last.event.Data.Error)
	assert.Equal(t, "working", last.activity)
}

// TestMuseFixture_ModelEndCarriesErrorOnly records the muse-code 1.4.20
// gap: a 400 turn fires no Stop, only PostLLMCall with error. A model-end
// error does not set harness_error (the harness may retry), so the hook
// path cannot report it; the reason is on the model-end for the
// first-turn watchdog's pending error.
func TestMuseFixture_ModelEndCarriesErrorOnly(t *testing.T) {
	steps := replayErrorFixture(t, mappingDialect(t, "muse-code"), loadErrorFixture(t, "muse-code", "api-error-400-1.4.20.jsonl"))
	var modelEndErrors int
	for _, s := range steps {
		assert.NotEqual(t, hooks.EventAgentEnd, s.event.Name)
		assert.NotEqual(t, "harness_error", s.activity)
		if s.event.Name == hooks.EventModelEnd {
			assert.Equal(t, "API error 400: "+fixtureProviderText+" (invalid_request_error)", s.event.Data.Error)
			modelEndErrors++
		}
	}
	assert.Equal(t, 3, modelEndErrors)
}

// TestHubHandler_SubagentAgentErrorSendsNothing checks that a sub-agent's
// failed turn changes nothing, locally or on the Hub.
func TestHubHandler_SubagentAgentErrorSendsNothing(t *testing.T) {
	sh, hh, reports := hubRecorderSetup(t)
	writeAgentInfoJSON(t, sh, `{"phase":"running","activity":"executing"}`)
	event := &hooks.Event{Name: hooks.EventAgentError, Dialect: "claude", Data: hooks.EventData{
		SessionID: "parent", Error: "API Error: 400", Raw: map[string]interface{}{"agent_id": "child"},
	}}

	require.NoError(t, sh.Handle(event))
	require.NoError(t, hh.Handle(event))

	assert.Equal(t, "executing", readAgentInfoMap(t, sh.StatusPath)["activity"])
	assert.Empty(t, reports())
}

// TestStatusHandler_AgentErrorWithoutTextSetsHarnessError checks that an
// agent-error with no error text still sets harness_error, with the bare
// source as the reason.
func TestStatusHandler_AgentErrorWithoutTextSetsHarnessError(t *testing.T) {
	h := newTempStatusHandler(t)
	writeAgentInfoJSON(t, h, `{"phase":"running","activity":"thinking"}`)

	require.NoError(t, h.Handle(&hooks.Event{Name: hooks.EventAgentError, Dialect: "grok-build"}))

	info := readAgentInfoMap(t, h.StatusPath)
	assert.Equal(t, "harness_error", info["activity"])
	assert.Equal(t, "model API", detailMessage(info))
}

// assertNoWaitingForInput checks that no step set waiting_for_input, locally
// or on the Hub, and that no Hub report carried the provider text.
func assertNoWaitingForInput(t *testing.T, steps []errorFixtureStep, providerText string) {
	t.Helper()
	for _, s := range steps {
		assert.NotEqual(t, "waiting_for_input", s.activity, s.event.Name)
		for _, r := range s.reports {
			assert.NotEqual(t, "waiting_for_input", r.Activity, s.event.Name)
			assert.NotContains(t, r.Message, providerText, s.event.Name)
		}
	}
}

// TestCopilotFixture_FatalErrorIsAFailedModelCall covers copilot 1.0.112's
// fatal 400: errorOccurred is a model-end carrying the provider's message
// (the reason the first-turn watchdog's pending error records), not a
// question for the user, so the agent never shows waiting_for_input and the
// Hub never gets the raw provider body as a message. No agentStop follows,
// so the hook path does not set harness_error.
func TestCopilotFixture_FatalErrorIsAFailedModelCall(t *testing.T) {
	steps := replayErrorFixture(t, mappingDialect(t, "copilot"), loadErrorFixture(t, "copilot", "api-error-400-1.0.112.jsonl"))
	require.Equal(t, []string{hooks.EventSessionStart, hooks.EventPromptSubmit, hooks.EventModelEnd}, eventNames(steps))
	assertNoWaitingForInput(t, steps, fixtureProviderText)
	last := steps[len(steps)-1]
	assert.Contains(t, last.event.Data.Error, fixtureProviderText)
	assert.NotEqual(t, "harness_error", last.activity)
}

// TestCopilotFixture_RetriedErrorHasNoInputNeededFlicker covers a 503 that
// copilot retried to success: the failed call's errorOccurred no longer
// flips the agent to waiting_for_input (which notified input-needed
// subscribers) before agentStop ends the turn in working.
func TestCopilotFixture_RetriedErrorHasNoInputNeededFlicker(t *testing.T) {
	steps := replayErrorFixture(t, mappingDialect(t, "copilot"), loadErrorFixture(t, "copilot", "transient-503-then-200-1.0.112.jsonl"))
	require.Equal(t, []string{
		hooks.EventSessionStart, hooks.EventPromptSubmit, hooks.EventModelEnd, hooks.EventAgentEnd,
	}, eventNames(steps))
	assertNoWaitingForInput(t, steps, "The service is currently unavailable.")
	assert.Equal(t, "working", steps[len(steps)-1].activity)
}

// TestCopilotFixture_RetriedErrorDoesNotCountModelCalls checks that the
// errorOccurred of a 503 copilot retried to success is uncounted: with
// max_model_calls set to 1 the replay does not reach limits_exceeded, and
// the model-call count stays 0. Copilot sends no hook for a successful
// model call, so counting its failures would stop an agent for retries.
func TestCopilotFixture_RetriedErrorDoesNotCountModelCalls(t *testing.T) {
	signals := stubSignalInit(t)
	scrubHubEnv(t)
	dir := t.TempDir()
	limitsPath := filepath.Join(dir, "agent-limits.json")
	statusPath := filepath.Join(dir, "agent-info.json")
	require.NoError(t, InitLimitsFile(limitsPath, 0, 1, 0, 0))
	require.NoError(t, os.WriteFile(statusPath, []byte(`{"phase":"running","activity":"working"}`), 0o644))
	lh := &LimitsHandler{
		maxModelCalls:   1,
		limitsPath:      limitsPath,
		triggerFilePath: filepath.Join(dir, "scion-limits-exceeded"),
		statusHandler:   &StatusHandler{StatusPath: statusPath},
	}

	d := mappingDialect(t, "copilot")
	var modelEnds int
	for _, rec := range loadErrorFixture(t, "copilot", "transient-503-then-200-1.0.112.jsonl") {
		event, err := d.Parse(rec)
		require.NoError(t, err)
		if event.Name == hooks.EventModelEnd {
			modelEnds++
			assert.True(t, event.Data.Uncounted)
		}
		require.NoError(t, lh.Handle(event))
	}

	assert.Equal(t, 1, modelEnds)
	assert.Equal(t, 0, readLimitsFile(t, limitsPath).ModelCallCount)
	assert.Empty(t, signals.get())
	assert.NotEqual(t, "limits_exceeded", readAgentInfo(t, statusPath).Activity)
}
