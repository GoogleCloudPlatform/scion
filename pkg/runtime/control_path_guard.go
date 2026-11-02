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

package runtime

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// namedVolumePattern matches a container runtime's named volume (Docker and
// Podman: a name with no path separator). A named volume lives in the
// runtime's own storage, not at a host path, so it cannot reach a Runtime
// Broker control file.
var namedVolumePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]+$`)

// isNamedVolume reports whether a bind source is a named volume rather than
// a host path.
func isNamedVolume(src string) bool {
	return !filepath.IsAbs(src) && namedVolumePattern.MatchString(src)
}

// logControlPathRefusal records the operator-facing detail of a refused
// mount source; the error returned to the requester carries no resolved
// host path.
func logControlPathRefusal(err error) {
	var re *brokercontrol.RefusalError
	if errors.As(err, &re) {
		runtimeLog().Warn("Refused a mount source that reaches Runtime Broker control files",
			"code", brokercontrol.ErrorCode, "detail", re.Detail())
	}
}

// checkBindSource is the last check on every host-path bind before it is
// emitted: the source may not reach a named Runtime Broker control path or
// contain the Scion global directory. Where a source inside the global
// directory may come from is decided earlier, by the code that knows what
// kind of mount it is (configured volumes: checkConfiguredVolumeSource;
// workspace and home: their own guards; auth files and development binaries:
// checkHostFileSource). It returns the resolved source, which is what must
// be mounted. Named volumes are returned unchanged.
func checkBindSource(src string) (string, error) {
	if isNamedVolume(src) {
		return src, nil
	}
	resolved, err := brokercontrol.CheckNamed(src)
	if err != nil {
		logControlPathRefusal(err)
		return "", err
	}
	return resolved, nil
}

// checkConfiguredVolumeSource applies the control-path check to a configured
// volume (settings, profiles, templates, harness configs, shared dirs, the
// Hub's inline config): inside the Scion global directory only the
// agent-data subtrees of brokercontrol.VolumeAllowed are allowed. Read-only
// volumes are checked the same way.
func checkConfiguredVolumeSource(src string) error {
	if isNamedVolume(src) {
		return nil
	}
	if _, err := brokercontrol.Check(src, brokercontrol.VolumeAllowed); err != nil {
		logControlPathRefusal(err)
		return err
	}
	return nil
}

// checkHostFileSource applies the control-path check to a host path that is
// copied or bound into an agent and never legitimately comes from the Scion
// global directory (harness auth files, development binaries). It returns
// the resolved source.
func checkHostFileSource(src string) (string, error) {
	resolved, err := brokercontrol.Check(src, nil)
	if err != nil {
		logControlPathRefusal(err)
		return "", err
	}
	return resolved, nil
}

// expandVolumeEnv expands environment variables in a volume path (e.g.
// ${GOPATH}, $HOME). Mount paths are visible to the agent, so names refused
// for agents expand to empty.
func expandVolumeEnv(path string) string {
	expanded, _, refused := util.ExpandEnvRefusing(path, util.IsAgentRefusedHostEnvName)
	for _, name := range refused {
		slog.Warn("volume path: host environment variable is reserved and is not expanded", "variable", name)
	}
	return expanded
}

// expandHostSource expands a configured volume source on the host side the
// way buildCommonRunArgs does: environment variables, then a leading "~".
func expandHostSource(path string) string {
	expanded := expandVolumeEnv(path)
	hostHome, _ := os.UserHomeDir()
	if strings.HasPrefix(expanded, "~/") {
		return filepath.Join(hostHome, expanded[2:])
	}
	if expanded == "~" {
		return hostHome
	}
	return expanded
}

// CheckVolumeSources applies the configured-volume control-path check to
// every host-path volume in vols, with each source expanded as it will be
// when the agent's container is created. It is for configuration time: a
// caller about to persist or start an agent configuration refuses it here,
// before anything is written or run. Volume types that name no broker-host
// path (gcs, cloudrun-volume, gke-shared-volume) are skipped, and so is a
// volume at /workspace: that is the agent's workspace, judged where the
// workspace is decided.
func CheckVolumeSources(vols []api.VolumeMount) error {
	for i, v := range vols {
		switch strings.ToLower(v.Type) {
		case "gcs", "cloudrun-volume", "gke-shared-volume":
			continue
		}
		if v.Target == "/workspace" {
			// The workspace mount is resolved, filtered and validated where
			// the workspace is decided (ValidateWorkspaceSource, which
			// includes the control-path check), and every bind is checked
			// again when the container is created.
			continue
		}
		if err := checkConfiguredVolumeSource(expandHostSource(v.Source)); err != nil {
			return fmt.Errorf("volumes[%d] (target %s): %w", i, v.Target, err)
		}
	}
	return nil
}

// CheckWorkspaceVolumeSources applies the named control-path check and the
// global-directory floor (checkBindSource, no allow list) to a volume at
// /workspace in vols. It is for a request that is about to be persisted
// (the Hub's create and start): which workspace shapes are allowed is
// decided later, where the workspace is resolved, but a workspace that
// reaches a control path is refused before anything is written.
func CheckWorkspaceVolumeSources(vols []api.VolumeMount) error {
	for i, v := range vols {
		if v.Target != "/workspace" {
			continue
		}
		switch strings.ToLower(v.Type) {
		case "gcs", "cloudrun-volume", "gke-shared-volume":
			continue
		}
		if _, err := checkBindSource(expandHostSource(v.Source)); err != nil {
			return fmt.Errorf("volumes[%d] (target %s): %w", i, v.Target, err)
		}
	}
	return nil
}
