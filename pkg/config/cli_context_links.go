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
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// Link entries: the known-project entries with source "link" and a local
// path. 'scion hub link' (and the registration of a project on a hub
// selected with 'scion use') records which hub a project directory is
// linked to as a link entry, so the directory keeps resolving to that hub
// whatever hub is current. There is one link entry per local path.

// LinkPathForProject returns the local path a link entry records for the
// project directory projectDir (a .scion directory, or the external
// settings directory of a project whose marker points there): the
// workspace directory holding .scion. The same function computes the path
// when a link entry is written and when it is looked up, so the two always
// agree.
func LinkPathForProject(projectDir string) string {
	if projectDir == "" {
		return ""
	}
	dir := cleanLinkPath(projectDir)
	sep := string(filepath.Separator)
	if strings.Contains(dir, sep+ProjectConfigsDir+sep) {
		if s, err := LoadSettings(dir); err == nil && s != nil && s.WorkspacePath != "" {
			return cleanLinkPath(s.WorkspacePath)
		}
		return dir
	}
	if filepath.Base(dir) == DotScion {
		return filepath.Dir(dir)
	}
	return dir
}

// cleanLinkPath returns path absolute, cleaned and with symlinks resolved
// where possible.
func cleanLinkPath(path string) string {
	p := filepath.Clean(path)
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return p
}

// SetLinkEntry records that the project at localPath (see
// LinkPathForProject) is linked to project p on the hub hubName. A link
// entry for the same path is replaced. When the known list already has an
// entry for that hub and project without a local path, it becomes the link
// entry, keeping its other fields.
func (c *CLIContext) SetLinkEntry(hubName string, p KnownProject, localPath string, now time.Time) error {
	if c.Hubs[hubName] == nil {
		return fmt.Errorf("no hub named %q", hubName)
	}
	if p.ID == "" {
		return errors.New("project has no ID")
	}
	if localPath == "" {
		return errors.New("link has no local path")
	}
	c.RemoveLinkEntry(localPath)
	stamp := now.UTC().Truncate(time.Second)
	for i := range c.Projects {
		e := &c.Projects[i]
		if e.Hub != hubName || e.ID != p.ID || e.LocalPath != "" {
			continue
		}
		e.Source = KnownProjectSourceLink
		e.LocalPath = localPath
		if p.Slug != "" {
			e.Slug = p.Slug
		}
		if p.Name != "" {
			e.Name = p.Name
		}
		e.LastUsed = stamp
		return nil
	}
	p.Hub = hubName
	p.Source = KnownProjectSourceLink
	p.LocalPath = localPath
	p.LastUsed = stamp
	c.Projects = append(c.Projects, p)
	return nil
}

// RemoveLinkEntry removes the link entry for localPath and reports whether
// there was one. Entries from other sources are kept.
func (c *CLIContext) RemoveLinkEntry(localPath string) bool {
	if localPath == "" {
		return false
	}
	removed := false
	kept := c.Projects[:0]
	for _, p := range c.Projects {
		if p.Source == KnownProjectSourceLink && p.LocalPath == localPath {
			removed = true
			continue
		}
		kept = append(kept, p)
	}
	c.Projects = kept
	if len(c.Projects) == 0 {
		c.Projects = nil
	}
	return removed
}

// LinkEntryFor returns the link entry for localPath and its hub entry, or
// nil and nil when there is none or its hub has no entry.
func (c *CLIContext) LinkEntryFor(localPath string) (*KnownProject, *HubEntry) {
	if localPath == "" {
		return nil, nil
	}
	for i := range c.Projects {
		p := &c.Projects[i]
		if p.Source != KnownProjectSourceLink || p.LocalPath != localPath {
			continue
		}
		if h := c.Hubs[p.Hub]; h != nil && h.Endpoint != "" {
			return p, h
		}
	}
	return nil, nil
}
