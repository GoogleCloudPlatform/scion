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
	"errors"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
)

// fakeLeaveProjects is a ProjectService that records Leave calls.
type fakeLeaveProjects struct {
	hubclient.ProjectService
	calls []string
	dry   []bool
	res   *hubclient.LeaveProjectResult
	err   error
}

func (f *fakeLeaveProjects) Leave(_ context.Context, projectID string, dryRun bool) (*hubclient.LeaveProjectResult, error) {
	f.calls = append(f.calls, projectID)
	f.dry = append(f.dry, dryRun)
	return f.res, f.err
}

var leaveTestProject = &hubclient.Project{ID: "0b9a4c1e-2f3d-4e5a-8b6c-7d8e9f0a1b2c", Name: "My Project"}

func TestProjectLeave_ArgumentRequired(t *testing.T) {
	assert.Error(t, projectLeaveCmd.Args(projectLeaveCmd, nil), "a project argument is required")
	assert.Error(t, projectLeaveCmd.Args(projectLeaveCmd, []string{"a", "b"}))
	assert.NoError(t, projectLeaveCmd.Args(projectLeaveCmd, []string{"a"}))
}

func TestProjectLeave_NotConfirmedCallsNothing(t *testing.T) {
	svc := &fakeLeaveProjects{}
	err := runProjectLeave(context.Background(), svc, leaveTestProject, leaveNotConfirmable, &bytes.Buffer{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "-y")
	assert.Empty(t, svc.calls, "an unconfirmed leave never reaches the hub")
}

// TestProjectLeave_DeclinedIsCancelled: "no" at an interactive prompt
// cancels with a non-zero exit (an error) and no -y hint; without a
// terminal and without -y the usage error with the hint stays.
func TestProjectLeave_DeclinedIsCancelled(t *testing.T) {
	svc := &fakeLeaveProjects{}
	err := runProjectLeave(context.Background(), svc, leaveTestProject, leaveDeclined, &bytes.Buffer{})
	require.ErrorIs(t, err, errProjectLeaveCancelled)
	assert.Contains(t, err.Error(), "cancelled")
	assert.NotContains(t, err.Error(), "-y")
	assert.Empty(t, svc.calls, "a declined leave never reaches the hub")

	assert.Equal(t, leaveConfirmed, leaveConfirmationOf(true, true))
	assert.Equal(t, leaveConfirmed, leaveConfirmationOf(true, false), "-y without a terminal")
	assert.Equal(t, leaveDeclined, leaveConfirmationOf(false, true))
	assert.Equal(t, leaveNotConfirmable, leaveConfirmationOf(false, false))
}

func TestProjectLeave_LeavesAndPrints(t *testing.T) {
	origFormat := outputFormat
	t.Cleanup(func() { outputFormat = origFormat })
	outputFormat = ""

	svc := &fakeLeaveProjects{res: &hubclient.LeaveProjectResult{
		ProjectID: leaveTestProject.ID, RemovedRoles: []string{"project-member"}, RemainingAccess: "none",
		Agents:             hubclient.LeaveProjectAgents{Count: 2},
		ProjectBoundTokens: hubclient.LeaveProjectTokenCounts{Revoked: 1},
		WillBeHeld:         true,
	}}
	var out bytes.Buffer
	require.NoError(t, runProjectLeave(context.Background(), svc, leaveTestProject, leaveConfirmed, &out))
	assert.Equal(t, []string{leaveTestProject.ID}, svc.calls)
	assert.Equal(t, []bool{false}, svc.dry, "the minimal command performs a real leave")
	assert.Contains(t, out.String(), "Left project My Project")
	assert.Contains(t, out.String(), "2 of your agents")
	assert.Contains(t, out.String(), "1 access tokens")
}

func TestProjectLeave_NotFoundMessage(t *testing.T) {
	svc := &fakeLeaveProjects{err: &apiclient.APIError{StatusCode: 404, Code: "not_found", Message: "Project not found"}}
	err := runProjectLeave(context.Background(), svc, leaveTestProject, leaveConfirmed, &bytes.Buffer{})
	require.Error(t, err)
	assert.Equal(t, "not a member of My Project (or it doesn't exist)", err.Error())
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

// TestProjectLeave_NotAvailableInAgentMode: an agent must not drop its
// user's membership, so 'project leave' is not in agentAllowed and agent
// mode removes it.
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

// TestProjectLeave_Registered: the command is wired under 'scion project'.
func TestProjectLeave_Registered(t *testing.T) {
	assert.Equal(t, projectCmd, projectLeaveCmd.Parent())
	assert.NotNil(t, projectLeaveCmd.Flags().ShorthandLookup("y"))
}
