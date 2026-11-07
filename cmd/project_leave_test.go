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
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
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
	err := runProjectLeave(context.Background(), svc, leaveTestProject, false, &bytes.Buffer{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "-y")
	assert.Empty(t, svc.calls, "an unconfirmed leave never reaches the hub")
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
	require.NoError(t, runProjectLeave(context.Background(), svc, leaveTestProject, true, &out))
	assert.Equal(t, []string{leaveTestProject.ID}, svc.calls)
	assert.Equal(t, []bool{false}, svc.dry, "the minimal command performs a real leave")
	assert.Contains(t, out.String(), "Left project My Project")
	assert.Contains(t, out.String(), "2 of your agents")
	assert.Contains(t, out.String(), "1 access tokens")
}

func TestProjectLeave_NotFoundMessage(t *testing.T) {
	svc := &fakeLeaveProjects{err: &apiclient.APIError{StatusCode: 404, Code: "not_found", Message: "Project not found"}}
	err := runProjectLeave(context.Background(), svc, leaveTestProject, true, &bytes.Buffer{})
	require.Error(t, err)
	assert.Equal(t, "not a member of My Project (or it doesn't exist)", err.Error())
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
