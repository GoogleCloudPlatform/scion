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

package config

import (
	"fmt"
	"strings"
	"time"
)

// The one-time import of a linked project's hub into the hub list. A
// project linked before link entries existed records its hub only as
// hub.endpoint in its own settings (or inherits the global one). The CLI no
// longer reads hub.endpoint, so the first time it resolves such a project
// it records the hub as a link entry instead. No settings file is changed,
// so an older binary still finds the endpoint there.

// ProjectImported reports whether the hub of the project at localPath (see
// LinkPathForProject) was already imported (see LinkEntryForProject).
func (c *CLIContext) ProjectImported(localPath string) bool {
	if c.Imported == nil {
		return false
	}
	_, ok := c.Imported.Projects[localPath]
	return ok
}

// markProjectImported records that the hub of the project at localPath was
// imported.
func (c *CLIContext) markProjectImported(localPath string, now time.Time) {
	if c.Imported == nil {
		c.Imported = &CLIContextImports{}
	}
	if c.Imported.Projects == nil {
		c.Imported.Projects = map[string]time.Time{}
	}
	c.Imported.Projects[localPath] = now.UTC().Truncate(time.Second)
}

// projectHubLink is what a project's own settings files say about its link
// to a hub.
type projectHubLink struct {
	// Linked reports whether the project was linked to a hub.
	Linked bool
	// ProjectID is the ID the hub knows the project by: hub.projectId,
	// else project_id.
	ProjectID string
	// Endpoint and Transport are the project's own hub.endpoint and
	// hub.transport ("" and nil when its settings set none).
	Endpoint  string
	Transport *HubTransportConfig
}

// projectHubLinkSettings reads the project's own settings files: the
// project directory and, for a git project with split storage (or a
// directory left by the removed 'scion hub shadow'), its external settings
// directory, the latter winning. The global settings and environment
// variables are not applied. A project counts as linked when hub.linked is
// true, or, with hub.linked unset, when it is a shadow directory or, in an
// unversioned file, carries a hub project ID that differs from its local
// project ID (a link made before hub.linked was recorded), as
// EnsureHubReady decides it. A versioned file keeps one ID, at
// hub.project_id. Tests replace it.
var projectHubLinkSettings = func(projectDir string) projectHubLink {
	dirs := []string{projectDir}
	if ext := GetProjectConfigDir(projectDir); ext != "" && ext != projectDir {
		dirs = append(dirs, ext)
	}
	var l projectHubLink
	var linked *bool
	var hubID, localID string
	shadow := false
	for _, dir := range dirs {
		if versioned, _ := detectDirSettingsFormat(dir); versioned {
			vs, err := LoadSingleFileVersioned(dir)
			if err != nil || vs == nil {
				continue
			}
			if vs.ProjectType == string(ProjectTypeShadow) {
				shadow = true
			}
			if vs.Hub == nil {
				continue
			}
			if vs.Hub.Linked != nil {
				linked = vs.Hub.Linked
			}
			if vs.Hub.ProjectID != "" {
				hubID, localID = vs.Hub.ProjectID, vs.Hub.ProjectID
			}
			if ep := strings.TrimSpace(vs.Hub.Endpoint); ep != "" {
				l.Endpoint = ep
			}
			if t := vs.Hub.Transport; t != nil {
				l.Transport = &HubTransportConfig{Mode: t.Mode, Audience: t.Audience}
			}
			continue
		}
		s, err := LoadSettingsFromDir(dir)
		if err != nil || s == nil {
			continue
		}
		if s.ProjectID != "" {
			localID = s.ProjectID
		}
		if s.Hub == nil {
			continue
		}
		if s.Hub.Linked != nil {
			linked = s.Hub.Linked
		}
		if s.Hub.ProjectID != "" {
			hubID = s.Hub.ProjectID
		}
		if ep := strings.TrimSpace(s.Hub.Endpoint); ep != "" {
			l.Endpoint = ep
		}
		if s.Hub.Transport != nil {
			l.Transport = s.Hub.Transport
		}
	}
	switch {
	case linked != nil:
		l.Linked = *linked
	case shadow:
		l.Linked = true
	default:
		l.Linked = hubID != "" && hubID != localID
	}
	l.ProjectID = hubID
	if l.ProjectID == "" {
		l.ProjectID = localID
	}
	return l
}

// LinkEntryForProject returns the link entry of the project at projectDir
// and its hub entry, or nil and nil when it has none. When the project has
// no link entry and its hub was never imported, it runs that import once:
//
//   - a linked project (see projectHubLinkSettings) gets a link entry for
//     its own hub.endpoint, or, without one, for the global settings'
//     hub.endpoint; the hub gets an entry (named by baseName) when it has
//     none, and the transport of the settings the endpoint came from is
//     copied onto an entry without one;
//   - the import is recorded for the project's link path even when there
//     was nothing usable to import (no valid endpoint or no project ID), so
//     its settings are not read again, and a link entry removed later
//     ('scion hub unlink', 'scion hub remove') is not brought back.
//
// A project that is not linked is not recorded and nothing is written. The
// global directory is never imported here (the global import covers it),
// and neither is a context file written by a newer version.
//
// The import is written under the lock, and c is updated to the saved
// context. When it cannot be saved, c holds the import anyway and the error
// wraps ErrCLIContextImport.
func LinkEntryForProject(c *CLIContext, projectDir string, baseName func(endpoint string) string) (*KnownProject, *HubEntry, error) {
	return linkEntryForProject(c, projectDir, baseName, true)
}

// LinkEntryForProjectInMemory is LinkEntryForProject without the save: c
// gets the pending one-time import of the project's link, and nothing is
// written (ptone/scion#5483).
func LinkEntryForProjectInMemory(c *CLIContext, projectDir string, baseName func(endpoint string) string) (*KnownProject, *HubEntry) {
	p, h, _ := linkEntryForProject(c, projectDir, baseName, false)
	return p, h
}

func linkEntryForProject(c *CLIContext, projectDir string, baseName func(endpoint string) string, save bool) (*KnownProject, *HubEntry, error) {
	localPath := LinkPathForProject(projectDir)
	if localPath == "" {
		return nil, nil, nil
	}
	if p, h := c.LinkEntryFor(localPath); h != nil {
		return p, h, nil
	}
	if c.Version > CLIContextVersion || c.ProjectImported(localPath) || IsGlobalProjectDir(projectDir) {
		return nil, nil, nil
	}
	link := projectHubLinkSettings(projectDir)
	if !link.Linked {
		return nil, nil, nil
	}
	endpoint, transport := link.Endpoint, link.Transport
	if endpoint == "" {
		endpoint, transport = globalHubSettings()
	}
	if endpoint != "" && ValidateHubURL(endpoint) != nil {
		endpoint = ""
	}
	importInto := func(target *CLIContext, now time.Time) {
		if _, h := target.LinkEntryFor(localPath); h != nil || target.ProjectImported(localPath) {
			return
		}
		if endpoint != "" && link.ProjectID != "" {
			name, _ := target.EnsureHub(endpoint, baseName(endpoint))
			setTransportIfUnset(target.Hubs[name], transport)
			// SetLinkEntry cannot fail here: the hub entry, the ID and
			// the path are all set.
			_ = target.SetLinkEntry(name, KnownProject{ID: link.ProjectID}, localPath, now)
		}
		target.markProjectImported(localPath, now)
	}
	if !save {
		importInto(c, time.Now())
		p, h := c.LinkEntryFor(localPath)
		return p, h, nil
	}
	var saved *CLIContext
	err := UpdateCLIContext(func(fresh *CLIContext) error {
		importInto(fresh, time.Now())
		saved = fresh
		return nil
	})
	if err != nil {
		importInto(c, time.Now())
		p, h := c.LinkEntryFor(localPath)
		return p, h, fmt.Errorf("%w: %w", ErrCLIContextImport, err)
	}
	*c = *saved
	p, h := c.LinkEntryFor(localPath)
	return p, h, nil
}
