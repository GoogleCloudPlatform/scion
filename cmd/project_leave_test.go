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

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
)

// fakeLeaveProjects is a ProjectService that answers the dry run with dry
// (or dryErr) and the real leave with res (or err), and records each call.
type fakeLeaveProjects struct {
	hubclient.ProjectService
	calls  []string
	dry    []bool
	dryRes *hubclient.LeaveProjectResult
	dryErr error
	res    *hubclient.LeaveProjectResult
	err    error
}

func (f *fakeLeaveProjects) Leave(_ context.Context, projectID string, dryRun bool) (*hubclient.LeaveProjectResult, error) {
	f.calls = append(f.calls, projectID)
	f.dry = append(f.dry, dryRun)
	if dryRun {
		if f.dryErr != nil {
			return nil, f.dryErr
		}
		return f.dryRes, nil
	}
	return f.res, f.err
}

var leaveTestProject = &hubclient.Project{ID: "0b9a4c1e-2f3d-4e5a-8b6c-7d8e9f0a1b2c", Name: "My Project"}

// leaveResult is a LeaveProjectResult with the given roles, remaining
// access, agents and token count.
func leaveResult(dryRun bool, roles []string, remaining string, agents []hubclient.LeaveProjectAgentItem, tokens int) *hubclient.LeaveProjectResult {
	if agents == nil {
		agents = []hubclient.LeaveProjectAgentItem{}
	}
	return &hubclient.LeaveProjectResult{
		ProjectID: leaveTestProject.ID, DryRun: dryRun, RemovedRoles: roles, RemainingAccess: remaining,
		Agents:             hubclient.LeaveProjectAgents{Count: len(agents), Items: agents},
		ProjectBoundTokens: hubclient.LeaveProjectTokenCounts{Revoked: tokens},
		WillBeHeld:         remaining == "none" && len(agents) > 0,
	}
}

var leaveTestAgents = []hubclient.LeaveProjectAgentItem{
	{ID: "a-1", Name: "alpha", Phase: "running"},
	{ID: "a-2", Name: "beta", Phase: "stopped"},
}

// leaveHarness drives runProjectLeave with recorded prompts, confirmations
// and deletes.
type leaveHarness struct {
	out       bytes.Buffer
	errOut    bytes.Buffer
	prompts   []string
	allowDel  []bool
	answer    leaveAgentChoice
	confirms  []string
	confirmed bool
	deletes   []string
	deleteErr map[string]error
	// outcomes are the delete outcomes by agent ID; an agent with no entry
	// gets a 204 (confirmed).
	outcomes map[string]hubDeleteOutcome
}

func (h *leaveHarness) io() leaveIO {
	return leaveIO{
		out:    &h.out,
		errOut: &h.errOut,
		prompt: func(q string, allowDelete bool) (leaveAgentChoice, error) {
			h.prompts = append(h.prompts, q)
			h.allowDel = append(h.allowDel, allowDelete)
			return h.answer, nil
		},
		confirm: func(q string) bool {
			h.confirms = append(h.confirms, q)
			return h.confirmed
		},
		deleteAgent: func(_ context.Context, id string, onAccepted func()) (hubDeleteOutcome, error) {
			h.deletes = append(h.deletes, id)
			if h.outcomes[id].Accepted && onAccepted != nil {
				onAccepted()
			}
			return h.outcomes[id], h.deleteErr[id]
		},
	}
}

func TestProjectLeave_ArgumentRequired(t *testing.T) {
	assert.Error(t, projectLeaveCmd.Args(projectLeaveCmd, nil), "a project argument is required")
	assert.Error(t, projectLeaveCmd.Args(projectLeaveCmd, []string{"a", "b"}))
	assert.NoError(t, projectLeaveCmd.Args(projectLeaveCmd, []string{"a"}))
}

// TestProjectLeave_DryRunFirstThenLeaves (acceptance 1): the dry run comes
// first and its consequences are printed; then the real leave.
func TestProjectLeave_DryRunFirstThenLeaves(t *testing.T) {
	svc := &fakeLeaveProjects{
		dryRes: leaveResult(true, []string{"project-member"}, "none", nil, 2),
		res:    leaveResult(false, []string{"project-member"}, "none", nil, 2),
	}
	h := &leaveHarness{}
	require.NoError(t, runProjectLeave(context.Background(), svc, leaveTestProject, leaveOptions{yes: true}, h.io()))
	assert.Equal(t, []bool{true, false}, svc.dry, "the dry run, then the leave")
	out := h.out.String()
	assert.Contains(t, out, "Leaving My Project:")
	assert.Contains(t, out, "Removes your direct roles: project-member")
	assert.Contains(t, out, "You lose access to the project")
	assert.Contains(t, out, "2 access tokens limited to this project will be revoked")
	assert.Contains(t, out, "Left project My Project")
	assert.Contains(t, out, "2 access tokens limited to this project were revoked")
	assert.Empty(t, h.prompts)
	assert.Empty(t, h.deletes)
}

// TestProjectLeave_EveryBuiltInRoleLeaves (acceptance 1): a member, an
// admin, a non-last owner and a viewer leave the same way, and the removed
// role is shown.
func TestProjectLeave_EveryBuiltInRoleLeaves(t *testing.T) {
	for _, role := range []string{"project-member", "project-admin", "project-owner", "project-viewer"} {
		t.Run(role, func(t *testing.T) {
			svc := &fakeLeaveProjects{
				dryRes: leaveResult(true, []string{role}, "none", nil, 0),
				res:    leaveResult(false, []string{role}, "none", nil, 0),
			}
			h := &leaveHarness{}
			require.NoError(t, runProjectLeave(context.Background(), svc, leaveTestProject, leaveOptions{yes: true}, h.io()))
			assert.Contains(t, h.out.String(), "Removes your direct roles: "+role)
			assert.Contains(t, h.out.String(), "Left project My Project")
			assert.Equal(t, []bool{true, false}, svc.dry)
		})
	}
}

// TestProjectLeave_DryRunFlagChangesNothing: --dry-run prints the
// consequences (or the dry-run LeaveResult as JSON) and never leaves.
func TestProjectLeave_DryRunFlagChangesNothing(t *testing.T) {
	dry := leaveResult(true, []string{"project-viewer"}, "none", leaveTestAgents, 1)
	svc := &fakeLeaveProjects{dryRes: dry}
	h := &leaveHarness{}
	require.NoError(t, runProjectLeave(context.Background(), svc, leaveTestProject, leaveOptions{dryRun: true}, h.io()))
	assert.Equal(t, []bool{true}, svc.dry, "only the dry run")
	out := h.out.String()
	assert.Contains(t, out, "Removes your direct roles: project-viewer")
	assert.Contains(t, out, "Your 2 agents in the project will be stopped and put on hold")
	assert.Contains(t, out, "- alpha (running)")
	assert.Contains(t, out, "- beta (stopped)")
	assert.NotContains(t, out, "Left project")
	assert.Empty(t, h.prompts)
	assert.Empty(t, h.confirms)
	assert.Empty(t, h.deletes)

	svc = &fakeLeaveProjects{dryRes: dry}
	h = &leaveHarness{}
	require.NoError(t, runProjectLeave(context.Background(), svc, leaveTestProject, leaveOptions{dryRun: true, json: true}, h.io()))
	assert.Equal(t, []bool{true}, svc.dry)
	var got hubclient.LeaveProjectResult
	require.NoError(t, json.Unmarshal(h.out.Bytes(), &got), h.out.String())
	assert.Equal(t, *dry, got, "--json prints the hub's LeaveResult")
}

// TestProjectLeave_JSONPrintsLeaveResult: with --json the real leave's
// LeaveResult is the whole output.
func TestProjectLeave_JSONPrintsLeaveResult(t *testing.T) {
	res := leaveResult(false, []string{"project-member"}, "group", nil, 0)
	svc := &fakeLeaveProjects{dryRes: leaveResult(true, []string{"project-member"}, "group", nil, 0), res: res}
	h := &leaveHarness{}
	require.NoError(t, runProjectLeave(context.Background(), svc, leaveTestProject, leaveOptions{yes: true, json: true}, h.io()))
	var got hubclient.LeaveProjectResult
	require.NoError(t, json.Unmarshal(h.out.Bytes(), &got), h.out.String())
	assert.Equal(t, *res, got)
}

// TestProjectLeave_HeldAgentsNeedAChoice: with agents that would be held,
// -y or a run without a terminal needs --delete-agents or --keep-agents,
// and nothing but the dry run is sent.
func TestProjectLeave_HeldAgentsNeedAChoice(t *testing.T) {
	for name, opts := range map[string]leaveOptions{
		"-y":          {yes: true},
		"no terminal": {},
	} {
		t.Run(name, func(t *testing.T) {
			svc := &fakeLeaveProjects{dryRes: leaveResult(true, []string{"project-member"}, "none", leaveTestAgents, 0)}
			h := &leaveHarness{}
			err := runProjectLeave(context.Background(), svc, leaveTestProject, opts, h.io())
			require.Error(t, err)
			var usage *usageError
			assert.ErrorAs(t, err, &usage)
			assert.Contains(t, err.Error(), "--delete-agents or --keep-agents")
			assert.Equal(t, []bool{true}, svc.dry, "only the dry run")
			assert.Empty(t, h.deletes)
		})
	}
}

// TestProjectLeave_PromptDeleteKeepCancel: on a terminal the caller with
// agents that would be held chooses Delete, Keep or Cancel; the answer is
// the confirmation.
func TestProjectLeave_PromptDeleteKeepCancel(t *testing.T) {
	run := func(answer leaveAgentChoice) (*fakeLeaveProjects, *leaveHarness, error) {
		svc := &fakeLeaveProjects{
			dryRes: leaveResult(true, []string{"project-member"}, "none", leaveTestAgents, 0),
			res:    leaveResult(false, []string{"project-member"}, "none", nil, 0),
		}
		h := &leaveHarness{answer: answer}
		err := runProjectLeave(context.Background(), svc, leaveTestProject, leaveOptions{interactive: true}, h.io())
		return svc, h, err
	}

	svc, h, err := run(leaveAgentsDelete)
	require.NoError(t, err)
	require.Len(t, h.prompts, 1)
	assert.Contains(t, h.prompts[0], "delete your 2 agents first")
	assert.Empty(t, h.confirms, "the agent prompt is the confirmation")
	assert.Equal(t, []string{"a-1", "a-2"}, h.deletes)
	assert.Equal(t, []bool{true, false}, svc.dry)
	assert.Contains(t, h.out.String(), "Deleted 2 agents: alpha, beta")

	svc, h, err = run(leaveAgentsKeep)
	require.NoError(t, err)
	assert.Empty(t, h.deletes)
	assert.Equal(t, []bool{true, false}, svc.dry)

	svc, h, err = run(leaveAgentsCancel)
	require.ErrorIs(t, err, errProjectLeaveCancelled)
	assert.Empty(t, h.deletes)
	assert.Equal(t, []bool{true}, svc.dry, "a cancel never leaves")
}

// TestProjectLeave_DeleteFailureAbortsBeforeLeave: --delete-agents stops at
// the first failed delete and never leaves; an agent already gone counts
// as deleted.
func TestProjectLeave_DeleteFailureAbortsBeforeLeave(t *testing.T) {
	agents := append(append([]hubclient.LeaveProjectAgentItem{}, leaveTestAgents...), hubclient.LeaveProjectAgentItem{ID: "a-3", Name: "gamma"})
	svc := &fakeLeaveProjects{dryRes: leaveResult(true, []string{"project-member"}, "none", agents, 0)}
	h := &leaveHarness{deleteErr: map[string]error{
		"a-1": &apiclient.APIError{StatusCode: 404, Code: "not_found", Message: "Agent not found"},
		"a-2": &apiclient.APIError{StatusCode: 500, Code: "internal_error", Message: "boom"},
	}}
	err := runProjectLeave(context.Background(), svc, leaveTestProject, leaveOptions{yes: true, agents: leaveAgentsDelete}, h.io())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "deleted alpha; failed to delete agent beta, so the project was not left")
	assert.Equal(t, []string{"a-1", "a-2"}, h.deletes, "stops at the first failure")
	assert.Equal(t, []bool{true}, svc.dry, "never leaves after a failed delete")

	svc = &fakeLeaveProjects{
		dryRes: leaveResult(true, []string{"project-member"}, "none", leaveTestAgents, 0),
		res:    leaveResult(false, []string{"project-member"}, "none", nil, 0),
	}
	h = &leaveHarness{deleteErr: map[string]error{"a-1": &apiclient.APIError{StatusCode: 404, Code: "not_found"}}}
	require.NoError(t, runProjectLeave(context.Background(), svc, leaveTestProject, leaveOptions{yes: true, agents: leaveAgentsDelete}, h.io()))
	assert.Equal(t, []string{"a-1", "a-2"}, h.deletes)
	assert.Equal(t, []bool{true, false}, svc.dry, "a 404 on delete is an agent already gone")
}

// TestProjectLeave_DeleteRefusedWhenListCut: when the hub cut the agent
// list, --delete-agents cannot delete every agent, so nothing is deleted
// and the project is not left.
func TestProjectLeave_DeleteRefusedWhenListCut(t *testing.T) {
	dry := leaveResult(true, []string{"project-member"}, "none", leaveTestAgents, 0)
	dry.Agents.Count, dry.Agents.Truncated = 120, true
	svc := &fakeLeaveProjects{dryRes: dry}
	h := &leaveHarness{}
	err := runProjectLeave(context.Background(), svc, leaveTestProject, leaveOptions{yes: true, agents: leaveAgentsDelete}, h.io())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "more than can be listed here")
	assert.Empty(t, h.deletes)
	assert.Equal(t, []bool{true}, svc.dry)
	assert.Contains(t, h.out.String(), "... and 118 more")
}

// TestProjectLeave_KeepAgentsFlag: -y --keep-agents leaves without
// deleting; the result says the agents will be held.
func TestProjectLeave_KeepAgentsFlag(t *testing.T) {
	svc := &fakeLeaveProjects{
		dryRes: leaveResult(true, []string{"project-member"}, "none", leaveTestAgents, 0),
		res:    leaveResult(false, []string{"project-member"}, "none", leaveTestAgents, 0),
	}
	h := &leaveHarness{}
	require.NoError(t, runProjectLeave(context.Background(), svc, leaveTestProject, leaveOptions{yes: true, agents: leaveAgentsKeep}, h.io()))
	assert.Empty(t, h.deletes)
	assert.Equal(t, []bool{true, false}, svc.dry)
	assert.Contains(t, h.out.String(), "Your 2 agents in the project will be stopped and put on hold.")
}

// TestProjectLeave_RemainingAccessHoldsNothing: with access kept through a
// group, agents are not held, so no choice is asked and --delete-agents
// deletes nothing.
func TestProjectLeave_RemainingAccessHoldsNothing(t *testing.T) {
	svc := &fakeLeaveProjects{
		dryRes: leaveResult(true, []string{"project-member"}, "group", leaveTestAgents, 0),
		res:    leaveResult(false, []string{"project-member"}, "group", leaveTestAgents, 0),
	}
	h := &leaveHarness{}
	require.NoError(t, runProjectLeave(context.Background(), svc, leaveTestProject, leaveOptions{yes: true, agents: leaveAgentsDelete}, h.io()))
	assert.Empty(t, h.deletes)
	assert.Empty(t, h.prompts)
	out := h.out.String()
	assert.Contains(t, out, "You keep access through a group")
	assert.Contains(t, out, "--delete-agents deletes nothing")
	assert.NotContains(t, out, "put on hold:")
}

// TestProjectLeave_RefusalsKeepGuidance (acceptance 2): the last owner and
// a group-only member get the hub's refusal with its guidance, nothing is
// left, and --json reports the error code.
func TestProjectLeave_RefusalsKeepGuidance(t *testing.T) {
	lastOwner := &apiclient.APIError{StatusCode: 409, Code: "last_owner", Message: "cannot remove or demote the last project owner — at least one usable (active, existing) owner must remain"}
	groupOnly := &apiclient.APIError{StatusCode: 409, Code: "membership_group_derived",
		Message: "You're a member of this project through a group. Leave the group, or ask a group admin to remove you.",
		Details: map[string]interface{}{"groups": []interface{}{
			map[string]interface{}{"id": "g-2", "name": "platform"},
			map[string]interface{}{"id": "g-1", "name": "eng"},
		}}}

	svc := &fakeLeaveProjects{dryErr: lastOwner}
	h := &leaveHarness{}
	err := runProjectLeave(context.Background(), svc, leaveTestProject, leaveOptions{yes: true}, h.io())
	require.Error(t, err)
	assert.Equal(t, "cannot leave My Project: "+lastOwner.Message+". Transfer ownership to another member first, or delete the project.", err.Error())
	assert.Equal(t, []bool{true}, svc.dry)

	svc = &fakeLeaveProjects{dryErr: groupOnly}
	err = runProjectLeave(context.Background(), svc, leaveTestProject, leaveOptions{yes: true}, (&leaveHarness{}).io())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "through a group")
	assert.Contains(t, err.Error(), "Groups: eng, platform")

	svc = &fakeLeaveProjects{dryErr: groupOnly}
	h = &leaveHarness{}
	err = runProjectLeave(context.Background(), svc, leaveTestProject, leaveOptions{yes: true, json: true}, h.io())
	require.Error(t, err)
	err = reportLeaveErrorJSON(&h.out, err)
	assert.True(t, isReportedInJSON(err), "the error is in the JSON output")
	var got struct {
		Error struct {
			Code    string                 `json:"code"`
			Message string                 `json:"message"`
			Details map[string]interface{} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(h.out.Bytes(), &got), h.out.String())
	assert.Equal(t, "membership_group_derived", got.Error.Code)
	assert.Contains(t, got.Error.Message, groupOnly.Message)
	assert.NotEmpty(t, got.Error.Details["groups"])
}

// TestProjectLeave_DryRun404IsNotNotAMember (R2): the project was already
// resolved, so a 404 from the leave route never reads "not a member (or it
// doesn't exist)". The experiment gate's "route not found" names
// hub.project_self_leave (code experiment_disabled); the handler's
// "Project not found" says there is no direct role.
func TestProjectLeave_DryRun404IsNotNotAMember(t *testing.T) {
	for name, tc := range map[string]struct {
		apiErr   *apiclient.APIError
		code     string
		message  string
		mentions string
	}{
		"experiment off": {&apiclient.APIError{StatusCode: 404, Code: "not_found", Message: "route not found"},
			"experiment_disabled", "cannot leave My Project: project self-leave (hub.project_self_leave) is not enabled on this Hub", "hub.project_self_leave"},
		"no direct role": {&apiclient.APIError{StatusCode: 404, Code: "not_found", Message: "Project not found"},
			"not_found", "cannot leave My Project: you have no direct role on it", "no direct role"},
		"hub without the route": {&apiclient.APIError{StatusCode: 404, Code: "not_found", Message: "Project route not found"},
			"unsupported", "cannot leave My Project: this Hub does not support project leave (it may be an older version)", "does not support"},
	} {
		t.Run(name, func(t *testing.T) {
			svc := &fakeLeaveProjects{dryErr: tc.apiErr}
			err := runProjectLeave(context.Background(), svc, leaveTestProject, leaveOptions{yes: true}, (&leaveHarness{}).io())
			require.Error(t, err)
			assert.Equal(t, tc.message, err.Error())
			assert.NotContains(t, err.Error(), "not a member")
			assert.Contains(t, err.Error(), tc.mentions)
			assert.Equal(t, []bool{true}, svc.dry)

			var out bytes.Buffer
			assert.True(t, isReportedInJSON(reportLeaveErrorJSON(&out, err)))
			assert.JSONEq(t, `{"error":{"code":"`+tc.code+`","message":"`+tc.message+`"}}`, out.String())
		})
	}
}

// TestProjectLeave_JSONReportsEveryCLIError (R1): with --json every error
// the command returns is one {"error": {...}} document on stdout and a
// jsonReportedError: a resolve miss (not_found), a usage error, a cancel,
// and a failed agent delete (agent_delete_failed, naming the agents
// already deleted and the one that failed).
func TestProjectLeave_JSONReportsEveryCLIError(t *testing.T) {
	type doc struct {
		Error struct {
			Code    string                 `json:"code"`
			Message string                 `json:"message"`
			Details map[string]interface{} `json:"details"`
		} `json:"error"`
	}
	report := func(err error) doc {
		var out bytes.Buffer
		rerr := reportLeaveErrorJSON(&out, err)
		require.True(t, isReportedInJSON(rerr))
		assert.Equal(t, err.Error(), rerr.Error())
		var d doc
		require.NoError(t, json.Unmarshal(out.Bytes(), &d), out.String())
		return d
	}

	d := report(projectLeaveResolveError("my-project", &apiclient.APIError{StatusCode: 404, Code: "not_found", Message: "Project not found"}))
	assert.Equal(t, "not_found", d.Error.Code)
	assert.Equal(t, "not a member of my-project (or it doesn't exist)", d.Error.Message)

	svc := &fakeLeaveProjects{dryRes: leaveResult(true, []string{"project-member"}, "none", leaveTestAgents, 0)}
	err := runProjectLeave(context.Background(), svc, leaveTestProject, leaveOptions{yes: true, json: true}, (&leaveHarness{}).io())
	d = report(err)
	assert.Equal(t, "usage", d.Error.Code)
	assert.Contains(t, d.Error.Message, "--delete-agents or --keep-agents")

	svc = &fakeLeaveProjects{dryRes: leaveResult(true, []string{"project-member"}, "none", nil, 0)}
	err = runProjectLeave(context.Background(), svc, leaveTestProject, leaveOptions{interactive: true, json: true}, (&leaveHarness{confirmed: false}).io())
	assert.Equal(t, "cancelled", report(err).Error.Code)

	svc = &fakeLeaveProjects{dryRes: leaveResult(true, []string{"project-member"}, "none", leaveTestAgents, 0)}
	h := &leaveHarness{deleteErr: map[string]error{"a-2": &apiclient.APIError{StatusCode: 500, Code: "internal_error", Message: "boom"}}}
	err = runProjectLeave(context.Background(), svc, leaveTestProject, leaveOptions{yes: true, json: true, agents: leaveAgentsDelete}, h.io())
	d = report(err)
	assert.Equal(t, "agent_delete_failed", d.Error.Code)
	assert.Equal(t, []interface{}{"alpha"}, d.Error.Details["deleted"])
	assert.Equal(t, "beta", d.Error.Details["failed"])
	assert.Equal(t, []bool{true}, svc.dry, "never leaves after a failed delete")
	assert.Empty(t, h.out.String(), "nothing but the error document is written on stdout")
}

// TestProjectLeave_AcceptedDeleteCountsOnlyWhenConfirmed (B1): a delete the
// hub accepted (202) counts as done only when the poll confirms it. A
// failed, not-taken, unobservable or timed-out delete stops before the
// leave, and the error names what was already deleted.
func TestProjectLeave_AcceptedDeleteCountsOnlyWhenConfirmed(t *testing.T) {
	accepted := func(o hubclient.DeletionWaitOutcome) hubDeleteOutcome {
		return hubDeleteOutcome{Accepted: true, Wait: hubclient.DeletionWaitResult{Outcome: o}}
	}

	svc := &fakeLeaveProjects{
		dryRes: leaveResult(true, []string{"project-member"}, "none", leaveTestAgents, 0),
		res:    leaveResult(false, []string{"project-member"}, "none", nil, 0),
	}
	h := &leaveHarness{outcomes: map[string]hubDeleteOutcome{
		"a-1": accepted(hubclient.DeletionConfirmed), "a-2": accepted(hubclient.DeletionConfirmed),
	}}
	require.NoError(t, runProjectLeave(context.Background(), svc, leaveTestProject, leaveOptions{yes: true, agents: leaveAgentsDelete}, h.io()))
	assert.Equal(t, []bool{true, false}, svc.dry, "202 then confirmed: leaves")
	assert.Contains(t, h.out.String(), "Deleted 2 agents: alpha, beta")

	for name, tc := range map[string]struct {
		outcome hubDeleteOutcome
		says    string
	}{
		"failed": {hubDeleteOutcome{Accepted: true, Wait: hubclient.DeletionWaitResult{
			Outcome: hubclient.DeletionFailed, Deletion: &hubclient.DeletionInfo{Code: "runtime_error", Error: "runtime unavailable"}}},
			"delete failed on the Hub (runtime_error)"},
		"not taken":    {accepted(hubclient.DeletionNotTaken), "did not take effect"},
		"unobservable": {accepted(hubclient.DeletionUnobservable), "cannot observe completion"},
		"timed out":    {accepted(hubclient.DeletionTimedOut), "still running when the wait ended"},
	} {
		t.Run(name, func(t *testing.T) {
			svc := &fakeLeaveProjects{dryRes: leaveResult(true, []string{"project-member"}, "none", leaveTestAgents, 0)}
			h := &leaveHarness{outcomes: map[string]hubDeleteOutcome{"a-2": tc.outcome}}
			err := runProjectLeave(context.Background(), svc, leaveTestProject, leaveOptions{yes: true, agents: leaveAgentsDelete}, h.io())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "deleted alpha; failed to delete agent beta, so the project was not left")
			assert.Contains(t, err.Error(), tc.says)
			assert.Equal(t, []bool{true}, svc.dry, "never leaves")
			assert.NotContains(t, h.out.String(), "Deleted")
		})
	}
}

// TestProjectLeave_JSONPromptShowsConsequencesOnStderr (O2): with --json
// and a prompt to answer, the consequences go to stderr, and stdout keeps
// only the result document.
func TestProjectLeave_JSONPromptShowsConsequencesOnStderr(t *testing.T) {
	svc := &fakeLeaveProjects{
		dryRes: leaveResult(true, []string{"project-member"}, "none", leaveTestAgents, 0),
		res:    leaveResult(false, []string{"project-member"}, "none", leaveTestAgents, 0),
	}
	h := &leaveHarness{answer: leaveAgentsKeep}
	require.NoError(t, runProjectLeave(context.Background(), svc, leaveTestProject, leaveOptions{interactive: true, json: true}, h.io()))
	assert.Contains(t, h.errOut.String(), "Leaving My Project:")
	assert.Contains(t, h.errOut.String(), "- alpha (running)")
	var got hubclient.LeaveProjectResult
	require.NoError(t, json.Unmarshal(h.out.Bytes(), &got), "stdout is the result alone: %s", h.out.String())

	svc = &fakeLeaveProjects{
		dryRes: leaveResult(true, []string{"project-member"}, "none", nil, 0),
		res:    leaveResult(false, []string{"project-member"}, "none", nil, 0),
	}
	h = &leaveHarness{}
	require.NoError(t, runProjectLeave(context.Background(), svc, leaveTestProject, leaveOptions{yes: true, json: true}, h.io()))
	assert.Empty(t, h.errOut.String(), "no prompt, so nothing on stderr")
}

// errReader fails every read.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("broken input") }

// TestProjectLeave_AgentPromptInput (O3): the agent prompt reads from the
// given input. An end of input with no answer cancels; a read error cancels
// and is returned; an answer with no final newline counts.
func TestProjectLeave_AgentPromptInput(t *testing.T) {
	var out bytes.Buffer
	got, err := promptLeaveAgentChoice(strings.NewReader(""), &out, "Q?", true)
	require.NoError(t, err)
	assert.Equal(t, leaveAgentsCancel, got, "EOF with no answer cancels")
	assert.Contains(t, out.String(), "Q? [d]elete / [k]eep / [c]ancel: ")

	got, err = promptLeaveAgentChoice(errReader{}, &out, "Q?", true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "broken input")
	assert.Equal(t, leaveAgentsCancel, got, "a read error cancels")

	got, err = promptLeaveAgentChoice(strings.NewReader("k\n"), &out, "Q?", true)
	require.NoError(t, err)
	assert.Equal(t, leaveAgentsKeep, got)
	got, err = promptLeaveAgentChoice(strings.NewReader("d"), &out, "Q?", true)
	require.NoError(t, err)
	assert.Equal(t, leaveAgentsDelete, got, "an answer at EOF without a newline counts")

	svc := &fakeLeaveProjects{dryRes: leaveResult(true, []string{"project-member"}, "none", leaveTestAgents, 0)}
	lio := (&leaveHarness{}).io()
	lio.prompt = func(q string, allowDelete bool) (leaveAgentChoice, error) {
		return promptLeaveAgentChoice(errReader{}, &out, q, allowDelete)
	}
	err = runProjectLeave(context.Background(), svc, leaveTestProject, leaveOptions{interactive: true}, lio)
	require.Error(t, err)
	assert.Equal(t, []bool{true}, svc.dry, "a read error never leaves")
}

// TestProjectLeave_SingularCounts (O5): a count of 1 reads in the singular.
func TestProjectLeave_SingularCounts(t *testing.T) {
	one := []hubclient.LeaveProjectAgentItem{{ID: "a-1", Name: "alpha", Phase: "running"}}
	svc := &fakeLeaveProjects{dryRes: leaveResult(true, []string{"project-member"}, "none", one, 1)}
	h := &leaveHarness{}
	err := runProjectLeave(context.Background(), svc, leaveTestProject, leaveOptions{yes: true}, h.io())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "you have 1 agent in My Project")
	assert.Contains(t, h.out.String(), "Your 1 agent in the project will be stopped")
	assert.Contains(t, h.out.String(), "1 access token limited to this project will be revoked")
	assert.Equal(t, "2 agents", countNoun(2, "agent"))
}

// TestProjectLeave_JSONFlagConfirmShowsConsequences (round 2 F1): with
// --json, an agent flag and a terminal, the yes/no prompt is still shown,
// so the consequences (with the agent list) go to stderr first.
func TestProjectLeave_JSONFlagConfirmShowsConsequences(t *testing.T) {
	svc := &fakeLeaveProjects{
		dryRes: leaveResult(true, []string{"project-member"}, "none", leaveTestAgents, 0),
		res:    leaveResult(false, []string{"project-member"}, "none", nil, 0),
	}
	h := &leaveHarness{confirmed: true}
	require.NoError(t, runProjectLeave(context.Background(), svc, leaveTestProject,
		leaveOptions{interactive: true, json: true, agents: leaveAgentsDelete}, h.io()))
	assert.Equal(t, []string{`Leave project "My Project"?`}, h.confirms)
	assert.Contains(t, h.errOut.String(), "Leaving My Project:")
	assert.Contains(t, h.errOut.String(), "- alpha")
	assert.Equal(t, []string{"a-1", "a-2"}, h.deletes)
	var got hubclient.LeaveProjectResult
	require.NoError(t, json.Unmarshal(h.out.Bytes(), &got), "stdout is the result alone: %s", h.out.String())
}

// TestProjectLeave_SessionOnlyRefusal (round 2 F2): the session-only gate
// refuses an access token with 403 forbidden and details.credential
// session_required; the CLI says a login session is needed, and --json
// keeps the hub's code.
func TestProjectLeave_SessionOnlyRefusal(t *testing.T) {
	gate := &apiclient.APIError{StatusCode: 403, Code: "forbidden",
		Message: `this operation requires an interactive session; credential kind "uat" is not allowed`,
		Details: map[string]interface{}{"credential": "session_required", "reason": "x"}}
	svc := &fakeLeaveProjects{dryErr: gate}
	err := runProjectLeave(context.Background(), svc, leaveTestProject, leaveOptions{yes: true}, (&leaveHarness{}).io())
	require.Error(t, err)
	assert.Equal(t, "cannot leave My Project: leaving needs an interactive login session (scion hub auth login); access tokens are refused", err.Error())
	assert.Equal(t, []bool{true}, svc.dry)

	var out bytes.Buffer
	require.True(t, isReportedInJSON(reportLeaveErrorJSON(&out, err)))
	var d struct {
		Error struct {
			Code    string                 `json:"code"`
			Details map[string]interface{} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &d))
	assert.Equal(t, "forbidden", d.Error.Code)
	assert.Equal(t, "session_required", d.Error.Details["credential"])

	other := &apiclient.APIError{StatusCode: 403, Code: "forbidden", Message: "nope"}
	err = runProjectLeave(context.Background(), &fakeLeaveProjects{dryErr: other}, leaveTestProject, leaveOptions{yes: true}, (&leaveHarness{}).io())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "interactive login", "only the session-only gate's 403 gets that message")
}

// TestProjectLeave_AcceptedDeletePrintsProgress (round 2 N1): a delete the
// hub accepted prints a waiting line on stderr before the poll.
func TestProjectLeave_AcceptedDeletePrintsProgress(t *testing.T) {
	confirmed := hubDeleteOutcome{Accepted: true, Wait: hubclient.DeletionWaitResult{Outcome: hubclient.DeletionConfirmed}}
	svc := &fakeLeaveProjects{
		dryRes: leaveResult(true, []string{"project-member"}, "none", leaveTestAgents, 0),
		res:    leaveResult(false, []string{"project-member"}, "none", nil, 0),
	}
	h := &leaveHarness{outcomes: map[string]hubDeleteOutcome{"a-2": confirmed}}
	require.NoError(t, runProjectLeave(context.Background(), svc, leaveTestProject, leaveOptions{yes: true, agents: leaveAgentsDelete}, h.io()))
	assert.Equal(t, "Waiting for the Hub to delete beta...\n", h.errOut.String(), "only the accepted delete waits")
}

// TestProjectLeave_ProductionDeleteWaitsForConfirmation (round 2 N4): the
// production seam (leaveAgentDeleter) polls a 202. Against a hub whose
// delete is accepted and then fails, the outcome is not confirmed and the
// leave is never sent.
func TestProjectLeave_ProductionDeleteWaitsForConfirmation(t *testing.T) {
	fastDeletionWait(t)
	hub := &asyncDeleteHub{projectID: "proj-1", agentName: "a-1", replies: []getReply{{body: replyRuntime}}}
	srv := httptest.NewServer(http.HandlerFunc(hub.serve))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	del := leaveAgentDeleter(client.ProjectAgents(hub.projectID))

	accepted := false
	outcome, err := del(context.Background(), "a-1", func() { accepted = true })
	require.NoError(t, err)
	assert.True(t, accepted, "a 202 runs onAccepted")
	assert.False(t, outcome.Confirmed())
	assert.Equal(t, hubclient.DeletionFailed, outcome.Wait.Outcome)

	svc := &fakeLeaveProjects{dryRes: leaveResult(true, []string{"project-member"}, "none",
		[]hubclient.LeaveProjectAgentItem{{ID: "a-1", Name: "alpha", Phase: "running"}}, 0)}
	lio := (&leaveHarness{}).io()
	lio.deleteAgent = del
	err = runProjectLeave(context.Background(), svc, leaveTestProject, leaveOptions{yes: true, agents: leaveAgentsDelete}, lio)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to delete agent alpha, so the project was not left")
	assert.Contains(t, err.Error(), "runtime_error")
	assert.Equal(t, []bool{true}, svc.dry, "never leaves")
}

// TestProjectLeave_CutListPromptOffersNoDelete (round 2 N5): when the hub
// cut the agent list, the prompt says delete is unavailable and offers keep
// or cancel; a delete answer cancels.
func TestProjectLeave_CutListPromptOffersNoDelete(t *testing.T) {
	dry := leaveResult(true, []string{"project-member"}, "none", leaveTestAgents, 0)
	dry.Agents.Count, dry.Agents.Truncated = 120, true
	svc := &fakeLeaveProjects{dryRes: dry}
	h := &leaveHarness{answer: leaveAgentsCancel}
	require.ErrorIs(t, runProjectLeave(context.Background(), svc, leaveTestProject, leaveOptions{interactive: true}, h.io()), errProjectLeaveCancelled)
	require.Len(t, h.prompts, 1)
	assert.Contains(t, h.prompts[0], "cannot be deleted here; keep them on hold, or cancel?")
	assert.Equal(t, []bool{false}, h.allowDel)

	var out bytes.Buffer
	got, err := promptLeaveAgentChoice(strings.NewReader("d\n"), &out, "Q?", false)
	require.NoError(t, err)
	assert.Equal(t, leaveAgentsCancel, got, "delete is not offered, so a delete answer cancels")
	assert.Contains(t, out.String(), "Q? [k]eep / [c]ancel: ")
	assert.NotContains(t, out.String(), "[d]elete")
}

// TestProjectLeave_ConfirmationWithoutAgents: with no agents to hold, -y
// confirms; on a terminal the yes/no prompt decides; without a terminal
// and without -y nothing but the dry run is sent.
func TestProjectLeave_ConfirmationWithoutAgents(t *testing.T) {
	newSvc := func() *fakeLeaveProjects {
		return &fakeLeaveProjects{
			dryRes: leaveResult(true, []string{"project-member"}, "none", nil, 0),
			res:    leaveResult(false, []string{"project-member"}, "none", nil, 0),
		}
	}

	svc := newSvc()
	err := runProjectLeave(context.Background(), svc, leaveTestProject, leaveOptions{}, (&leaveHarness{}).io())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "-y")
	assert.Equal(t, []bool{true}, svc.dry, "an unconfirmed leave sends only the dry run")

	svc = newSvc()
	h := &leaveHarness{confirmed: false}
	err = runProjectLeave(context.Background(), svc, leaveTestProject, leaveOptions{interactive: true}, h.io())
	require.ErrorIs(t, err, errProjectLeaveCancelled)
	assert.NotContains(t, err.Error(), "-y")
	assert.Equal(t, []string{`Leave project "My Project"?`}, h.confirms)
	assert.Equal(t, []bool{true}, svc.dry)

	svc = newSvc()
	h = &leaveHarness{confirmed: true}
	require.NoError(t, runProjectLeave(context.Background(), svc, leaveTestProject, leaveOptions{interactive: true}, h.io()))
	assert.Equal(t, []bool{true, false}, svc.dry)
}

func TestProjectLeave_AgentAnswerParsing(t *testing.T) {
	for in, want := range map[string]leaveAgentChoice{
		"d\n": leaveAgentsDelete, "Delete": leaveAgentsDelete, " k ": leaveAgentsKeep, "KEEP\n": leaveAgentsKeep,
		"c": leaveAgentsCancel, "": leaveAgentsCancel, "yes": leaveAgentsCancel,
	} {
		assert.Equal(t, want, parseLeaveAgentAnswer(in), "%q", in)
	}
	assert.Equal(t, leaveAgentsDelete, leaveAgentFlagChoice(true, false))
	assert.Equal(t, leaveAgentsKeep, leaveAgentFlagChoice(false, true))
	assert.Equal(t, leaveAgentsUnset, leaveAgentFlagChoice(false, false))
}

// TestProjectLeave_ResolveErrorsOnlyNotFoundIsNotMember: resolving the
// project reads as "not a member" only for a not-found answer (a hub 404,
// or the resolver finding no match). An authentication failure, a server
// error, an ambiguous name and a token without the lookup scopes are
// returned with their own cause.
func TestProjectLeave_ResolveErrorsOnlyNotFoundIsNotMember(t *testing.T) {
	const notMember = "not a member of my-project (or it doesn't exist)"
	for name, err := range map[string]error{
		"hub 404":            &apiclient.APIError{StatusCode: 404, Code: "not_found", Message: "Project not found"},
		"resolver not found": &projectNotFoundError{ref: "my-project"},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, notMember, projectLeaveResolveError("my-project", err).Error())
		})
	}
	for name, cause := range map[string]error{
		"401":            &apiclient.APIError{StatusCode: 401, Code: "unauthorized", Message: "authentication required"},
		"500":            &apiclient.APIError{StatusCode: 500, Code: "internal_error", Message: "boom"},
		"ambiguous name": errors.New("multiple projects found with name 'my-project' - please use the project ID instead"),
		"token scopes":   hubsync.ScopedTokenProjectLookupEmptyError("my-project"),
	} {
		t.Run(name, func(t *testing.T) {
			got := projectLeaveResolveError("my-project", cause)
			require.Error(t, got)
			assert.NotEqual(t, notMember, got.Error())
			assert.NotContains(t, got.Error(), "not a member")
			assert.ErrorIs(t, got, cause, "the cause is wrapped, not replaced")
		})
	}
	var apiErr *apiclient.APIError
	require.ErrorAs(t, projectLeaveResolveError("my-project", &apiclient.APIError{StatusCode: 500}), &apiErr)
	assert.Equal(t, 500, apiErr.StatusCode)
}

// TestProjectLeave_NotAvailableInAgentMode (acceptance 7): an agent must
// not drop its user's membership, so 'project leave' is not in
// agentAllowed and agent mode removes it.
func TestProjectLeave_NotAvailableInAgentMode(t *testing.T) {
	assert.False(t, agentAllowed["project.leave"])

	t.Setenv("SCION_CLI_MODE", "agent")
	root := &cobra.Command{Use: "scion"}
	project := &cobra.Command{Use: "project"}
	project.AddCommand(&cobra.Command{Use: "leave"})
	root.AddCommand(project)
	applyModeRestrictions(root, resolveMode())
	assert.NotContains(t, collectCommandNames(root), "project.leave")
}

// TestProjectLeave_AvailableOnWindows (acceptance 7): the command makes
// Hub API calls only, so the Windows build runs it.
func TestProjectLeave_AvailableOnWindows(t *testing.T) {
	assert.True(t, windowsAllowed["project.leave"])
	assert.False(t, windowsAllowed["project"], "the parent stays gated")
}

// TestProjectLeave_Registered: the command and its flags are wired under
// 'scion project', and the two agent flags exclude each other.
func TestProjectLeave_Registered(t *testing.T) {
	assert.Equal(t, projectCmd, projectLeaveCmd.Parent())
	assert.NotNil(t, projectLeaveCmd.Flags().ShorthandLookup("y"))
	for _, name := range []string{"delete-agents", "keep-agents", "dry-run", "json"} {
		assert.NotNil(t, projectLeaveCmd.Flags().Lookup(name), name)
	}
	assert.Nil(t, projectLeaveCmd.Flags().Lookup("stop-agents"))

	t.Cleanup(func() {
		_ = projectLeaveCmd.Flags().Set("delete-agents", "false")
		_ = projectLeaveCmd.Flags().Set("keep-agents", "false")
		projectLeaveCmd.Flags().Lookup("delete-agents").Changed = false
		projectLeaveCmd.Flags().Lookup("keep-agents").Changed = false
	})
	require.NoError(t, projectLeaveCmd.Flags().Set("delete-agents", "true"))
	require.NoError(t, projectLeaveCmd.Flags().Set("keep-agents", "true"))
	err := projectLeaveCmd.ValidateFlagGroups()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "delete-agents")
}
