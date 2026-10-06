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

package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// imageProvenanceFile is the broker-side agent state file that records each
// image input ProvisionAgent saw. It lives in the agent dir next to
// scion-agent.json, outside every container mount (not in the agent home,
// where agent-info.json lives), so the image a later Start selects is
// resolved from broker-side agent state (ptone/scion#1799). Image provenance
// (including the provisioned profile) is recorded broker-side.
const imageProvenanceFile = config.ImageProvenanceFileName

// imageProvenanceVersion is written into every record. A record without it
// (or with another value) is rejected rather than half-trusted.
const imageProvenanceVersion = 1

// imageProvenance is the per-source image record. See settings-precedence.md,
// "Container image and Kubernetes image pull policy".
type imageProvenance struct {
	// Version is always imageProvenanceVersion; see readImageProvenance.
	Version int `json:"version"`
	// RequestImage is the user's explicit request image at provision time
	// (CLI --image, a local --config image the CLI promotes to it, or the
	// Hub's request image). Start replays it at the top tier when the
	// current request sets no image, so a first start and a later plain
	// restart rank it the same way.
	RequestImage string `json:"requestImage,omitempty"`
	// TemplateImage and TemplateImagePullPolicy are the template chain's
	// own image / kubernetes.imagePullPolicy at provision time, with no
	// inline, profile, settings or file contribution folded in. Start uses
	// them as the template tier only when the template can no longer be
	// resolved (e.g. a hub agent restarted on a broker with no local copy),
	// so a profile or settings pin removed since provision never lingers
	// disguised as the template's value.
	TemplateImage           string `json:"templateImage,omitempty"`
	TemplateImagePullPolicy string `json:"templateImagePullPolicy,omitempty"`
	// InlineImage and InlineImagePullPolicy are the create-time inline
	// config's own image / kubernetes.imagePullPolicy (the broker-side
	// counterparts of agent-info.json's display copies ExplicitImage /
	// ExplicitImagePullPolicy). Start falls back to them when the current
	// request's inline config does not set the field.
	InlineImage           string `json:"inlineImage,omitempty"`
	InlineImagePullPolicy string `json:"inlineImagePullPolicy,omitempty"`
	// Profile is the settings profile the agent was provisioned with
	// (after the active-profile fallback). Start uses only this profile to
	// look up the profile harness_overrides image and pull policy and the
	// profile image_registry; image selection reads no agent-info.json
	// field.
	//
	// An empty Profile means no profile was set or active at provision; the
	// profile active at start time then applies, as for any settings
	// lookup with no profile.
	Profile string `json:"profile,omitempty"`
	// Template is the template the agent was provisioned from (the same
	// value recorded, for display, as agent-info.json's template). Start
	// resolves the template-tier image and pull policy from this template
	// when the start request carries no absolute template path, so editing
	// agent-info.json cannot move the agent onto another template's image.
	Template string `json:"template,omitempty"`
	// TemplateHash is set when the template was loaded from a
	// content-addressed cache (the same value recorded, for display, as
	// agent-info.json's templateHash). Template is then a display name
	// only: Start does not look it up by name and treats the template as
	// unresolvable, so the recorded template-tier values apply.
	TemplateHash string `json:"templateHash,omitempty"`
}

func writeImageProvenance(agentDir string, p imageProvenance) error {
	p.Version = imageProvenanceVersion
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	// Mode 0600, written atomically (temp file + rename) so a reader never
	// sees a partial record.
	return writeAgentInfoFile(filepath.Join(agentDir, imageProvenanceFile), data, 0o600)
}

// readImageProvenance returns the recorded provenance. It returns (nil, nil)
// only when the file is genuinely absent — an agent provisioned before
// provenance was recorded — in which case Start uses its legacy behaviour.
// A file that exists but cannot be read, does not parse, or lacks the
// version marker is an error: Start fails rather than fall back to
// agent-info.json.
func readImageProvenance(agentDir string) (*imageProvenance, error) {
	if agentDir == "" {
		return nil, nil
	}
	path := filepath.Join(agentDir, imageProvenanceFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, imageProvenanceError(path, err)
	}
	var p imageProvenance
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, imageProvenanceError(path, err)
	}
	if p.Version != imageProvenanceVersion {
		return nil, imageProvenanceError(path, fmt.Errorf("unsupported or missing version %d (want %d)", p.Version, imageProvenanceVersion))
	}
	return &p, nil
}

// ImageProvenanceError reports an image-provenance.json that exists but is
// unusable (unreadable, unparseable, or without the version marker). Start,
// and a broker start/restart, fail with it rather than fall back to
// agent-info.json. Error() deliberately omits the host path, so a broker can
// return it to the Hub; Path is for logs.
type ImageProvenanceError struct {
	Path string
	Err  error
}

func (e *ImageProvenanceError) Error() string {
	return fmt.Sprintf("the agent's image provenance record is unusable (%v); refusing to fall back to agent-info.json: re-provision the agent (scion reincarnate, or delete and re-create it)", e.Err)
}

func (e *ImageProvenanceError) Unwrap() error { return e.Err }

// Is makes errors.Is(err, config.ErrAgentStateConflict) hold: an unusable
// record is an agent-state conflict (409, re-provision), distinct from an
// unavailable state directory.
func (e *ImageProvenanceError) Is(target error) bool {
	return target == config.ErrAgentStateConflict
}

func imageProvenanceError(path string, err error) error {
	return &ImageProvenanceError{Path: path, Err: withoutHostPath(err)}
}

// withoutHostPath replaces an *fs.PathError (whose message carries the host
// path) with its operation and cause, so an error that reaches a broker
// response does not reveal broker paths; callers keep the path separately
// for logs.
func withoutHostPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s: %w", pe.Op, pe.Err)
	}
	return err
}

// AgentStateDirError reports that a start or restart cannot locate the
// agent's broker-side state directory: a shared-workspace project with no
// determinable external agents root, or (on restart) an external agent dir
// that does not exist. The in-project agents root is never used instead
// (ptone/scion#1799). Error() carries no host path.
type AgentStateDirError struct {
	Path string
	Err  error
}

func (e *AgentStateDirError) Error() string {
	return fmt.Sprintf("the agent's broker-side state directory is unavailable (%v); refusing to use the in-project agents root: re-provision the agent (scion reincarnate, or delete and re-create it)", e.Err)
}

func (e *AgentStateDirError) Unwrap() error { return e.Err }

// Is makes errors.Is(err, config.ErrAgentStateDirUnavailable) hold for every
// AgentStateDirError, so brokers have a single mapping to 409.
func (e *AgentStateDirError) Is(target error) bool {
	return target == config.ErrAgentStateDirUnavailable
}

// ProvisionedProfile returns the settings profile recorded in the agent's
// broker-side image provenance. ok is false only when the agent has no
// provenance file (provisioned before it was recorded); a file that exists
// but is unusable is an *ImageProvenanceError, never a silent fallback.
//
// The agent dir is AgentStateDir's (strict, as for every broker caller): the
// same directory GetAgent and ProvisionAgent use, including the effective
// shared-workspace detection for an agent whose state is in the external
// agents root although the request does not carry sharedWorkspace, and with
// no fallback to the other agents root (ptone/scion#1799). For a
// shared-workspace agent, an undeterminable external root is an
// *AgentStateDirError, and so is a missing agent dir when mustExist (a
// restart) is set.
func ProvisionedProfile(projectPath, agentName string, sharedWorkspace bool, hubProjectID string, mustExist bool) (profile string, ok bool, err error) {
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	if err != nil {
		return "", false, nil
	}
	// The broker's callers are always strict (broker mode).
	agentDir, shared, err := AgentStateDir(projectDir, agentName, sharedWorkspace, hubProjectID, true)
	if err != nil {
		if shared {
			return "", false, &AgentStateDirError{Err: withoutHostPath(err)}
		}
		return "", false, nil
	}
	if shared && mustExist {
		if _, statErr := os.Stat(agentDir); statErr != nil {
			return "", false, &AgentStateDirError{Path: agentDir, Err: errors.New("external agent directory does not exist")}
		}
	}
	p, err := readImageProvenance(agentDir)
	if err != nil {
		return "", false, err
	}
	if p == nil {
		return "", false, nil
	}
	return p.Profile, true, nil
}

// logImageProvenanceError logs an *ImageProvenanceError with its host path,
// which the error's own message omits.
func logImageProvenanceError(agentName string, err error) {
	var pe *ImageProvenanceError
	if errors.As(err, &pe) {
		slog.Error("image provenance unusable", "agent", agentName, "path", pe.Path, "error", pe.Err)
	}
	var de *AgentStateDirError
	if errors.As(err, &de) {
		slog.Error("agent state directory unavailable", "agent", agentName, "path", de.Path, "error", de.Err)
	}
}
