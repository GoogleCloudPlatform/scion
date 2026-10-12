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
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Agent commands under a target-hub profile (ptone/scion#4240, X3b).
//
// create, start, list, stop, delete, look and logs run with a profile
// selected. They
// act on agents of one project on the target hub, named with --project
// <name or id>, as the test identity named with --as (and --run). They work
// only through the target hub's API:
//
//   - no local project is resolved and no local settings are read;
//   - no hub sync runs, and no sync state, agent name cache, agent
//     directory or worktree is written or removed;
//   - no local template is uploaded (--type must name a template on the
//     target hub), and no environment value is gathered from the local
//     environment (env-gather is off). --config and --task-file are
//     explicit input and are sent as given;
//   - flags whose behaviour needs local state or another credential
//     (--attach, --workspace, --upload-template, delete --cleanup-only,
//     the relationship filters of list) are refused.
//
// look and logs read the terminal output and the log through the hub's
// relay to the broker, like any hub-mode call. message is not available
// with a profile: it names the sender from this agent's own identity on its
// own hub (SCION_AGENT_NAME) and uses agent-only send paths.
//
// Without a profile these commands are unchanged, in agent mode too.

// hubProfileAgentCmds are the agent commands that run with a profile.
// Each has local --as and --run flags (sharing the hub tree's variables).
var hubProfileAgentCmds = []*cobra.Command{createCmd, startCmd, listCmd, stopCmd, deleteCmd, lookCmd, logsCmd}

// hubProfileAgentFlags are the --as and --run flags of hubProfileAgentCmds.
var hubProfileAgentFlags = map[*pflag.Flag]bool{}

func init() {
	for _, c := range hubProfileAgentCmds {
		c.Flags().StringVar(&hubProfileAs, "as", "", "Only with a hub profile: act on the target hub as this cached test identity of the run")
		c.Flags().StringVar(&hubProfileRun, "run", "", "Only with a hub profile: the run label of cached test identities (overrides SCION_HUB_RUN)")
		hubProfileAgentFlags[c.Flags().Lookup("as")] = true
		hubProfileAgentFlags[c.Flags().Lookup("run")] = true
	}
}

// errProfileNeedsProject is returned when an agent command under a profile
// names no project.
var errProfileNeedsProject = errors.New("with a hub profile, name the project on the target hub with --project <name or id>: no local project is used")

// profileAgentHubContext returns the hub context of an agent command under
// the selected profile: the profile client acting as the --as identity and
// the project named with --project, resolved on the target hub. It reads
// no local settings and runs no hub sync. ProjectPath stays empty, so the
// shared hub paths write no local sync state.
func profileAgentHubContext() (*HubContext, error) {
	if globalMode {
		return nil, errors.New("--global does not apply with a hub profile: name the project on the target hub with --project <name or id>")
	}
	ref := strings.TrimSpace(projectPath)
	if ref == "" {
		return nil, errProfileNeedsProject
	}
	s, err := openHubProfileSession(true)
	if err != nil {
		return nil, err
	}
	client, err := s.Client()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	project, err := resolveProjectNameOrID(ctx, client, ref)
	if err != nil {
		return nil, fmt.Errorf("project %q on the target hub of profile %q: %w", ref, hubProfileName(), err)
	}
	endpoint := s.Endpoint()
	return &HubContext{
		Client:        client,
		Endpoint:      endpoint,
		Settings:      &config.Settings{Hub: &config.HubClientConfig{Endpoint: endpoint, ProjectID: project.ID}},
		ProjectID:     project.ID,
		TargetProfile: hubProfileName(),
	}, nil
}

// profileLocalOnly reports whether hubCtx is a target-hub profile context,
// whose commands must not touch local project state.
func (h *HubContext) profileLocalOnly() bool {
	return h != nil && h.TargetProfile != ""
}

// checkProfileAgentFlags refuses, with a profile selected, the flags of an
// agent command whose behaviour needs local project state or a credential
// other than the profile's. It runs before any request.
func checkProfileAgentFlags(cmd *cobra.Command) error {
	if !hubProfileSelected() {
		return nil
	}
	const inferred = "the reference is inferred from this agent, which is not an agent on the target hub"
	refused := []struct{ name, why string }{
		{"attach", "attaching uses a terminal session credential that is not the profile's"},
		{"workspace", "it names a host path or a subdirectory of a local project"},
		{"upload-template", "local templates are not uploaded to the target hub"},
		{"cleanup-only", "it removes local agent files, and a profile uses none"},
		{"descendants", inferred},
		{"ancestors", inferred},
		{"lineage", inferred},
	}
	// list --all lists across projects; stop --all and delete --stopped
	// act on the named project and are allowed.
	if cmd == listCmd {
		refused = append(refused, struct{ name, why string }{"all", "name the project with --project"})
	}
	for _, r := range refused {
		f := cmd.Flags().Lookup(r.name)
		if f == nil || !f.Changed {
			continue
		}
		return fmt.Errorf("--%s is not available with a hub profile (%q): %s", r.name, hubProfileName(), r.why)
	}
	return nil
}
