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

//go:build unix

package migrate

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

// Identity binding of slug-keyed legacy trees (fs-cleanup-arch Q8 on
// ptone/scion#5819): a tree found by slug may belong to a deleted project
// whose slug a different project reuses, so it is migrated only when the
// identity markers it carries name the project's ID.
type identity int

const (
	identityVerified identity = iota
	identityUnverified
	identityMismatch
)

// maxMarkerBytes caps an identity marker read.
const maxMarkerBytes = 1 << 20

// Workspace record directories under the global dir that older hubs and
// brokers wrote for a hub-local project tree (pkg/config workspace records
// as of the phase-2 base: hub-workspaces/ by the hub, broker-workspaces/ by
// a broker), each holding a file named after the slug with the project ID.
// Phase 2 retires the writers; the records remain on upgraded hubs.
var workspaceRecordDirs = []string{"hub-workspaces", "broker-workspaces"}

// treeIdentity reports whether the legacy tree src belongs to projectID.
// Markers: <src>/.scion/project-id (and its pre-rename name), the project
// ID keys of <src>/.scion/settings.yaml (current and pre-rename, through
// pkg/projectkeys), and, for the hub-local tree, the workspace records
// under globalDir. Any marker naming another ID is a mismatch; no
// marker at all is unverified.
func treeIdentity(src Tree, local bool, globalDir, slug, projectID string) (identity, string, error) {
	var found []string
	add := func(where, id string) {
		if id = strings.TrimSpace(id); id != "" {
			found = append(found, where+"="+id)
			if id != projectID {
				found[len(found)-1] += " (mismatch)"
			}
		}
	}
	scion := filepath.Join(src.Rel, ".scion")
	checked := []string{}
	for _, file := range []string{projectkeys.ProjectIDFile, projectkeys.LegacyProjectIDFile} {
		checked = append(checked, ".scion/"+file)
		b, ok, err := readMarker(Tree{Base: src.Base, Rel: filepath.Join(scion, file)})
		if err != nil {
			return identityUnverified, "", err
		}
		if ok {
			add(".scion/"+file, string(b))
		}
	}
	checked = append(checked, ".scion/settings.yaml")
	if b, ok, err := readMarker(Tree{Base: src.Base, Rel: filepath.Join(scion, "settings.yaml")}); err != nil {
		return identityUnverified, "", err
	} else if ok {
		for _, id := range settingsProjectIDs(b) {
			add(".scion/settings.yaml", id)
		}
	}
	if local {
		for _, dir := range workspaceRecordDirs {
			checked = append(checked, filepath.Join(globalDir, dir, slug))
			b, ok, err := readMarker(Tree{Base: globalDir, Rel: filepath.Join(dir, slug)})
			if err != nil {
				return identityUnverified, "", err
			}
			if ok {
				add(dir+"/"+slug, string(b))
			}
		}
	}
	if len(found) == 0 {
		return identityUnverified, "no identity marker found; checked " + strings.Join(checked, ", "), nil
	}
	detail := "markers read: " + strings.Join(found, ", ") + "; project " + projectID
	for _, f := range found {
		if strings.HasSuffix(f, "(mismatch)") {
			return identityMismatch, detail, nil
		}
	}
	return identityVerified, detail, nil
}

// settingsProjectIDs returns the project IDs a settings.yaml names: under
// hub.project_id, hub.projectId and top-level project_id, and the
// pre-rename keys pkg/projectkeys names. Unparseable YAML names none.
func settingsProjectIDs(b []byte) []string {
	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil
	}
	var ids []string
	str := func(v any) {
		if s, ok := v.(string); ok && s != "" {
			ids = append(ids, s)
		}
	}
	// Current keys and, through pkg/projectkeys, the pre-rename ones.
	str(doc[projectkeys.ConfigProjectIDKey])
	str(doc[projectkeys.LegacyConfigProjectIDKey])
	hubKeys := []string{
		projectkeys.ConfigHubProjectIDKey, projectkeys.ConfigHubProjectIDJSON,
		projectkeys.LegacyConfigHubProjectIDKey, projectkeys.LegacyConfigHubProjectIDJSON,
	}
	if hub, ok := doc["hub"].(map[string]any); ok {
		for _, key := range hubKeys {
			str(hub[strings.TrimPrefix(key, "hub.")])
		}
	}
	for _, key := range hubKeys {
		str(doc[key]) // a flattened dotted key
	}
	return ids
}

// readMarker reads a small regular file without following a symlink at
// any component. ok is false when it does not exist; a symlink, a
// non-regular file or an oversized file is an error.
func readMarker(t Tree) ([]byte, bool, error) {
	parent, name, ok, err := openParent(t, false)
	if err != nil || !ok {
		if errors.Is(err, unix.ENOTDIR) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer closeFd(parent)
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("open identity marker %s: %w", t, err)
	}
	f := os.NewFile(uintptr(fd), name)
	defer func() { _ = f.Close() }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, false, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Size > maxMarkerBytes {
		return nil, false, fmt.Errorf("identity marker %s is not a small regular file", t)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxMarkerBytes))
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}
