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
	issuer := projectVisitorsAddCmd.Flags().Lookup("issuer")
	require.NotNil(t, issuer)
	assert.Equal(t, []string{"true"}, issuer.Annotations[cobra.BashCompOneRequiredFlag], "--issuer is required")
	for _, flag := range []string{"email", "domain"} {
		f := projectVisitorsAddCmd.Flags().Lookup(flag)
		require.NotNil(t, f, flag)
		assert.Nil(t, f.Annotations[cobra.BashCompOneRequiredFlag], "--%s alone is not required", flag)
	}
	require.NotNil(t, projectVisitorsRemoveCmd.Flags().Lookup("domain"), "remove names a rule by --domain")
}

// add takes exactly one of --email and --domain.
func TestProjectVisitorsCommand_AddEmailOrDomain(t *testing.T) {
	for name, args := range map[string][]string{
		"neither": {"--issuer", "https://hub-a.example.com"},
		"both":    {"--issuer", "https://hub-a.example.com", "--email", "ann@corp.example", "--domain", "corp.example"},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := &cobra.Command{Use: "add", RunE: func(*cobra.Command, []string) error { return nil }}
			var issuer, email, domain string
			cmd.Flags().StringVar(&issuer, "issuer", "", "")
			cmd.Flags().StringVar(&email, "email", "", "")
			cmd.Flags().StringVar(&domain, "domain", "", "")
			_ = cmd.MarkFlagRequired("issuer")
			cmd.MarkFlagsOneRequired("email", "domain")
			cmd.MarkFlagsMutuallyExclusive("email", "domain")
			cmd.SetArgs(args)
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			assert.Error(t, cmd.Execute())
		})
	}
	// The real command declares the same groups.
	email := projectVisitorsAddCmd.Flags().Lookup("email")
	domain := projectVisitorsAddCmd.Flags().Lookup("domain")
	require.NotNil(t, email)
	require.NotNil(t, domain)
	assert.Contains(t, email.Annotations["cobra_annotation_one_required"], "email domain")
	assert.Contains(t, email.Annotations["cobra_annotation_mutually_exclusive"], "email domain")
	assert.Contains(t, domain.Annotations["cobra_annotation_one_required"], "email domain")
}

func TestPrintProjectVisitors_SourcesAndRules(t *testing.T) {
	rules := []hubclient.ProjectVisitorRule{
		{ID: "rule-1", Issuer: "https://hub-a.example.com", Domain: "corp.example", IssuerAccepted: true},
		{ID: "rule-2", Issuer: "https://hub-b.example.com", Domain: "other.example"},
	}
	list := []hubclient.ProjectVisitor{
		{ID: "id-1", Email: "ann@corp.example", Issuer: "https://hub-a.example.com", Status: "active", IssuerAccepted: true, Source: "rule", SourceRuleID: "rule-1"},
		{ID: "id-2", Email: "bob@elsewhere.example", Issuer: "https://hub-a.example.com", Status: "pending", IssuerAccepted: true, Source: "email"},
	}
	var out strings.Builder
	require.NoError(t, printProjectVisitors(&out, list, rules))
	text := out.String()
	assert.Contains(t, text, "SOURCE")
	assert.Regexp(t, `ann@corp\.example\s+\S+\s+active\s+domain corp\.example`, text)
	assert.Regexp(t, `bob@elsewhere\.example\s+\S+\s+pending\s+email`, text)
	assert.Regexp(t, `corp\.example\s+https://hub-a\.example\.com\s+1\s+never\s+rule-1`, text)
	assert.Contains(t, text, "other.example (issuer not accepted)")

	out.Reset()
	require.NoError(t, printProjectVisitors(&out, nil, nil))
	assert.Equal(t, "No visitors.\n", out.String())
}

func TestMatchProjectVisitorRule(t *testing.T) {
	rules := []hubclient.ProjectVisitorRule{
		{ID: "rule-1", Issuer: "https://hub-a.example.com", Domain: "corp.example"},
		{ID: "rule-2", Issuer: "https://hub-b.example.com", Domain: "corp.example"},
		{ID: "rule-3", Issuer: "https://hub-a.example.com", Domain: "other.example"},
	}
	r, err := matchProjectVisitorRule(rules, "Other.Example", "")
	require.NoError(t, err)
	assert.Equal(t, "rule-3", r.ID)

	_, err = matchProjectVisitorRule(rules, "corp.example", "")
	var usage *usageError
	require.True(t, errors.As(err, &usage), "a domain with rules from two issuers needs --issuer: %v", err)

	r, err = matchProjectVisitorRule(rules, "corp.example", "https://hub-b.example.com/")
	require.NoError(t, err)
	assert.Equal(t, "rule-2", r.ID)

	_, err = matchProjectVisitorRule(rules, "sub.corp.example", "")
	assert.ErrorContains(t, err, "no visitor rule")
	assert.True(t, visitorListHasID([]hubclient.ProjectVisitor{{ID: "id-1"}}, " id-1 "))
	assert.False(t, visitorListHasID([]hubclient.ProjectVisitor{{ID: "id-1"}}, "rule-1"))
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
