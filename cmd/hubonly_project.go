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
	"fmt"
	"os"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/GoogleCloudPlatform/scion/pkg/tunnelclient"
)

// Where the project of tunnel, ssh or ssh-config came from.
const (
	hubOnlySourceFlag      = "flag"
	hubOnlySourceContext   = "context"
	hubOnlySourceDirectory = "directory"
)

// hubOnlyProject is the hub project tunnel, ssh and ssh-config use when the
// command line names none in an agent reference.
type hubOnlyProject struct {
	// ref is the project slug or id sent to the hub, or "".
	ref string
	// label names the project in messages: the --project value, the
	// context's slug or the directory project's name. Never a raw id the
	// user did not type.
	label string
	// source is one of the hubOnlySource constants, or "" when no project
	// applies.
	source string
	// unlinkedDir is the name of a directory project that is not linked to
	// a hub, or "". It hides the context project (see
	// resolveInvocationTarget), so it is an error where a project is needed.
	unlinkedDir string
}

// resolveHubOnlyProject returns the project for tunnel, ssh and ssh-config,
// after resolveInvocationTarget filled projectPath. The precedence is that
// of resolveInvocationTarget: --project, then the directory project, then
// the current context ('scion use'). The global settings are never a
// directory project, and a local project id is never used for a directory
// project that is not linked to a hub.
func resolveHubOnlyProject() hubOnlyProject {
	switch {
	case globalMode:
		// --global names the global settings, which never give these
		// commands a project; resolveInvocationTarget applies no context
		// project either.
		return hubOnlyProject{}
	case projectSource == targetSourceContext:
		return hubOnlyProject{ref: projectPath, label: contextProjectSlug, source: hubOnlySourceContext}
	case projectPath != "":
		// For these commands --project always names a hub project, also
		// when its value is global or home.
		return hubOnlyProject{ref: projectPath, label: projectPath, source: hubOnlySourceFlag}
	}
	dir, ok := filesystemProject()
	if !ok {
		return hubOnlyProject{}
	}
	name := config.GetProjectName(dir)
	ref := linkedHubProjectID(dir)
	if ref == "" {
		return hubOnlyProject{unlinkedDir: name}
	}
	return hubOnlyProject{ref: ref, label: name, source: hubOnlySourceDirectory}
}

// linkedHubProjectID returns the id the hub knows the directory project dir
// by: hub.projectId, or else project_id when the project's own settings say
// it is linked (registration keeps the local id when the hub accepts it).
// It returns "" for a project that is not linked. Only the project's own
// settings files are read, never the global ones.
func linkedHubProjectID(dir string) string {
	hubID, localID := ownProjectIDs(dir)
	if hubID != "" {
		return hubID
	}
	if linked := hubsync.ProjectLinkedSetting(dir, false); linked != nil && *linked {
		return localID
	}
	return ""
}

// ownProjectIDs returns hub.projectId and the project's own id from the
// settings files of the project dir itself and of its external config
// directory, read in that order with the later file winning, as
// hubsync.ProjectLinkedSetting reads hub.linked. Global settings are never
// read.
//
// A versioned settings file keeps the project's own id at hub.project_id
// only: when a dir's file is versioned, the own id is taken from there even
// when it is empty, and a legacy top-level project_id does not count. So
// when the external config directory's file is versioned without
// hub.project_id, a legacy project_id in the in-repo dir does not count
// either, and the directory is treated as not linked (it fails safe).
func ownProjectIDs(dir string) (hubID, localID string) {
	dirs := []string{dir}
	if ext := config.GetProjectConfigDir(dir); ext != "" && ext != dir {
		dirs = append(dirs, ext)
	}
	for _, d := range dirs {
		versioned := isVersionedSettingsDir(d)
		if s, err := config.LoadSettingsFromDir(d); err == nil && s != nil {
			// A versioned file keeps the project's own id at
			// hub.project_id, which the settings view also reports as
			// Hub.ProjectID; it is read in the versioned branch below,
			// where it counts only with hub.linked. So both ids are read
			// from the view for a legacy file only.
			if !versioned && s.Hub != nil && s.Hub.ProjectID != "" {
				hubID = s.Hub.ProjectID
			}
			// A versioned file keeps the id at hub.project_id only; a
			// top-level project_id left in it does not count.
			if !versioned && s.ProjectID != "" {
				localID = s.ProjectID
			}
		}
		if versioned {
			localID = ""
			if vs, err := config.LoadSingleFileVersioned(d); err == nil && vs != nil && vs.Hub != nil {
				localID = vs.Hub.ProjectID
			}
		}
	}
	return hubID, localID
}

// isVersionedSettingsDir reports whether dir's settings file is in the
// versioned format. A missing, unreadable or unparseable file reports
// false. It mirrors config.detectDirSettingsFormat, which is not exported:
// the file GetSettingsPath finds, classified by DetectSettingsFormat.
func isVersionedSettingsDir(dir string) bool {
	path := config.GetSettingsPath(dir)
	if path == "" {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	version, _ := config.DetectSettingsFormat(data)
	return version != ""
}

// describe names the project and its source for messages.
func (p hubOnlyProject) describe() string {
	switch p.source {
	case hubOnlySourceFlag:
		return fmt.Sprintf("project %s (from --project)", p.label)
	case hubOnlySourceContext:
		if p.label == "" {
			return "the project selected with scion use"
		}
		return fmt.Sprintf("project %s (from the scion use context)", p.label)
	case hubOnlySourceDirectory:
		return fmt.Sprintf("project %s (from the directory project)", p.label)
	}
	return "project"
}

// unlinkedError is the error for a directory project that is not linked to
// a hub, or nil.
func (p hubOnlyProject) unlinkedError() error {
	if p.unlinkedDir == "" {
		return nil
	}
	return tunnelclient.Errorf(tunnelclient.ExitUsage, "directory project %s is not linked to a hub; pass --project or run scion hub link", p.unlinkedDir)
}

// notFoundError is the error for a project the hub at endpoint does not
// know.
func (p hubOnlyProject) notFoundError(endpoint string) error {
	return tunnelclient.Errorf(tunnelclient.ExitAgentNotFound, "%s not found on hub %s", p.describe(), config.RedactURL(endpoint))
}
