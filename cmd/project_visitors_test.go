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
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

func TestProjectVisitorsCommand_Wiring(t *testing.T) {
	require.Same(t, projectCmd, projectVisitorsCmd.Parent(), "visitors is a project subcommand")
	var names []string
	for _, c := range projectVisitorsCmd.Commands() {
		names = append(names, c.Name())
	}
	sort.Strings(names)
	assert.Equal(t, []string{"add", "extend", "list", "remove"}, names)
	assert.Nil(t, projectVisitorsCmd.Flags().Lookup("domain"), "domain grants are not part of this command yet")
	assert.Nil(t, projectVisitorsAddCmd.Flags().Lookup("domain"), "domain grants are not part of this command yet")
	for _, flag := range []string{"issuer", "email"} {
		f := projectVisitorsAddCmd.Flags().Lookup(flag)
		require.NotNil(t, f, flag)
		assert.Equal(t, []string{"true"}, f.Annotations[cobra.BashCompOneRequiredFlag], "--%s is required", flag)
	}
}

// The visitor commands are human mode only: no agentAllowed entry names
// them, so agent mode removes them while keeping "project".
func TestProjectVisitorsCommand_NotAvailableInAgentMode(t *testing.T) {
	for path := range agentAllowed {
		assert.False(t, path == "project.visitors" || strings.HasPrefix(path, "project.visitors."),
			"agentAllowed must not list %q", path)
	}

	root := &cobra.Command{Use: "scion"}
	project := &cobra.Command{Use: "project"}
	visitors := &cobra.Command{Use: "visitors"}
	for _, c := range projectVisitorsCmd.Commands() {
		visitors.AddCommand(&cobra.Command{Use: c.Name(), Run: func(*cobra.Command, []string) {}})
	}
	project.AddCommand(visitors)
	root.AddCommand(project)

	applyModeRestrictions(root, ModeAgent)
	remaining := collectCommandNames(root)
	assert.Contains(t, remaining, "project")
	for _, name := range remaining {
		assert.False(t, strings.HasPrefix(name, "project.visitors"), "agent mode keeps %q", name)
	}
}

func TestMatchProjectVisitor(t *testing.T) {
	list := []hubclient.ProjectVisitor{
		{ID: "id-1", Email: "alice@corp.example", Issuer: "https://hub-a.example.com"},
		{ID: "id-2", Email: "alice@corp.example", Issuer: "https://hub-b.example.com"},
		{ID: "id-3", Email: "bob@corp.example", Issuer: "https://hub-a.example.com"},
	}

	v, err := matchProjectVisitor(list, "id-2", "")
	require.NoError(t, err)
	assert.Equal(t, "id-2", v.ID)

	v, err = matchProjectVisitor(list, "Bob@Corp.Example", "")
	require.NoError(t, err)
	assert.Equal(t, "id-3", v.ID)

	_, err = matchProjectVisitor(list, "alice@corp.example", "")
	var usage *usageError
	require.True(t, errors.As(err, &usage), "an email from two issuers needs --issuer: %v", err)

	v, err = matchProjectVisitor(list, "alice@corp.example", "https://hub-b.example.com/")
	require.NoError(t, err)
	assert.Equal(t, "id-2", v.ID)

	_, err = matchProjectVisitor(list, "carol@corp.example", "")
	assert.ErrorContains(t, err, "no visitor")
}

// --json is registered on the commands that print a visitor, so their
// JSON branches are reachable.
func TestProjectVisitorsCommand_JSONFlag(t *testing.T) {
	for _, c := range []*cobra.Command{projectVisitorsListCmd, projectVisitorsAddCmd, projectVisitorsExtendCmd} {
		f := c.Flags().Lookup("json")
		require.NotNil(t, f, "%s has --json", c.Name())
		assert.Equal(t, "bool", f.Value.Type(), c.Name())
	}
	assert.Nil(t, projectVisitorsRemoveCmd.Flags().Lookup("json"), "remove prints no visitor")

	savedFormat := outputFormat
	t.Cleanup(func() { projectVisitorsJSON, outputFormat = false, savedFormat })
	projectVisitorsJSON, outputFormat = false, ""
	assert.False(t, projectVisitorsWantJSON())
	require.NoError(t, projectVisitorsListCmd.Flags().Set("json", "true"))
	assert.True(t, projectVisitorsWantJSON())
}

func TestProjectVisitorsCommand_FormatStatus(t *testing.T) {
	assert.Equal(t, "active", formatVisitorStatus(hubclient.ProjectVisitor{Status: "active", IssuerAccepted: true}))
	assert.Equal(t, "pending (issuer not accepted)", formatVisitorStatus(hubclient.ProjectVisitor{Status: "pending"}))
}
