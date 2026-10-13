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
	"fmt"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/spf13/cobra"
)

// errHubShadowRemoved is returned by the hidden 'hub shadow' stub.
var errHubShadowRemoved = errors.New("'scion hub shadow' was removed; run 'scion use <project>' to work with a hub project from any directory")

// hubShadowRemovedCmd keeps 'scion hub shadow' as a hidden command that
// points to 'scion use' and fails. It is deleted together with the shadow
// directory read path (isShadowProject).
var hubShadowRemovedCmd = &cobra.Command{
	Use:                "shadow",
	Hidden:             true,
	DisableFlagParsing: true,
	// Skip root's pre-run hook: the stub reads no project, settings or
	// current context, so it gives the same answer everywhere.
	PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
	RunE: func(*cobra.Command, []string) error {
		return errHubShadowRemoved
	},
}

func init() {
	hubCmd.AddCommand(hubShadowRemovedCmd)
}

// shadowNoticeFormat is the migration notice for a directory created by the
// removed 'hub shadow'. Its arguments are the project name, the marker path
// and the project slug.
const shadowNoticeFormat = "This directory is a shadow of hub project '%s'; 'scion hub shadow' was removed. " +
	"Delete %s and run 'scion use %s' (works from any directory). " +
	"'scion project prune' then cleans up its settings.\n"

// printShadowNotice prints the shadow migration notice on cmd's stderr when
// the working directory resolves to a shadow marker. Root's pre-run hook
// calls it once per invocation, and only when the directory project is in
// effect.
func printShadowNotice(cmd *cobra.Command) {
	if cmd.Name() == "help" || cmd.Name() == cobra.ShellCompRequestCmd || cmd.Name() == cobra.ShellCompNoDescRequestCmd {
		return
	}
	markerPath := config.FindProjectMarkerPath()
	if markerPath == "" {
		return
	}
	marker, err := config.ReadProjectMarker(markerPath)
	if err != nil || !marker.IsShadow() {
		return
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), shadowNoticeFormat, marker.ProjectName, shadowMarkerDisplayPath(markerPath), marker.ProjectSlug)
}

// shadowMarkerDisplayPath returns markerPath relative to the working
// directory ("./.scion" when the marker is there), or as given when no
// relative form exists.
func shadowMarkerDisplayPath(markerPath string) string {
	wd, err := os.Getwd()
	if err != nil {
		return markerPath
	}
	rel, err := filepath.Rel(wd, markerPath)
	if err != nil {
		return markerPath
	}
	if rel == config.DotScion {
		return "./" + rel
	}
	return rel
}
