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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

// ErrHarnessConfigPolicy marks a launch refused by the harness-config policy
// attached to the context (config.ContextWithHarnessConfigPolicy). The
// policy's own error is wrapped alongside it, so callers can recover its
// details with errors.As.
var ErrHarnessConfigPolicy = errors.New("harness-config refused by policy")

// CheckHarnessConfigPolicy evaluates the context's harness-config policy, if
// any, against the harness-config launch has resolved. Policy is evaluated
// where launch resolves the harness-config: after template and
// harness-config resolution (resolveTemplateAndHarnessConfig, used by
// Preflight and ProvisionAgent) and at each harness construction
// (ProvisionAgent and Start), always with the effective entry
// (harness.EffectiveConfig) exactly as it will run. Returns nil when no
// policy is attached.
func CheckHarnessConfigPolicy(ctx context.Context, name string, entry config.HarnessConfigEntry) error {
	policy := config.HarnessConfigPolicyFromContext(ctx)
	if policy == nil {
		return nil
	}
	if err := policy(name, entry); err != nil {
		return fmt.Errorf("%w: %w", ErrHarnessConfigPolicy, err)
	}
	return nil
}

// ErrHarnessConfigNotEvaluated matches (errors.Is) a refusal when a
// harness-config policy is attached but the harness-config could not be
// evaluated while a provisioner wrapper is staged. The refusal itself is a
// *HarnessConfigNotEvaluatedError, which names the harness-config.
var ErrHarnessConfigNotEvaluated = errors.New("harness-config could not be evaluated by policy")

// HarnessConfigNotEvaluatedError names the harness-config the policy could
// not evaluate. Cause is the resolution error, for logs only.
type HarnessConfigNotEvaluatedError struct {
	Name  string
	Cause error
}

func (e *HarnessConfigNotEvaluatedError) Error() string {
	return fmt.Sprintf("harness-config %q could not be evaluated by policy: %v", e.Name, e.Cause)
}

// Is reports a match for ErrHarnessConfigNotEvaluated.
func (e *HarnessConfigNotEvaluatedError) Is(target error) bool {
	return target == ErrHarnessConfigNotEvaluated
}

// harnessAfterResolveError decides what Start does when harness.Resolve fails
// for harnessName. A staged provisioner runs only if the current launch
// resolved and staged it:
//   - a policy is attached and a provisioner wrapper is staged in agentHome:
//     refuse (ErrHarnessConfigPolicy, ErrHarnessConfigNotEvaluated), since
//     the entry could not be evaluated;
//   - otherwise (no policy, or nothing staged): clear the staged provisioning
//     state (resetStagedProvisioning) and fall back to
//     harness.New(harnessType), which never constructs a container-script
//     harness.
//
// No policy and "policy attached but entry not evaluable" stay distinct:
// only the latter refuses.
func harnessAfterResolveError(ctx context.Context, agentHome, harnessName, harnessType string, resolveErr error) (api.Harness, error) {
	if config.HarnessConfigPolicyFromContext(ctx) != nil && agentHome != "" && harness.HarnessProvisionHookStaged(agentHome) {
		return nil, fmt.Errorf("%w: %w", ErrHarnessConfigPolicy, &HarnessConfigNotEvaluatedError{Name: harnessName, Cause: resolveErr})
	}
	if err := resetStagedProvisioning(agentHome); err != nil {
		return nil, err
	}
	return harness.New(harnessType), nil
}

// resetStagedProvisioning clears the agent home's staged provisioning state
// before any harness is staged, with or without a policy: the provisioner
// wrapper and the whole staged bundle, inputs/ included
// (harness.ClearStagedProvisioning). Nothing a previous launch or copied home
// content left there is visible to this launch, and the only
// wrapper that can exist afterwards is one this launch's container-script
// Provision writes. The caller restages the control-plane inputs and
// secrets.
func resetStagedProvisioning(agentHome string) error {
	return harness.ClearStagedProvisioning(agentHome)
}

// controlPlaneInputsDirName is the directory, in the agent directory (outside
// every container mount, unlike the agent home), holding the control plane's copy
// of the per-agent inputs ProvisionAgent stages for a container-script
// harness (instructions, system prompt, resolved skills). It lies outside
// every container mount scion computes for the agent (pinned by
// pkg/runtime's TestHarnessInputsRecordOutsideScionMounts). Author-configured
// volumes are a separately tracked capability, outside the scope of this
// record.
const controlPlaneInputsDirName = config.HarnessInputsRecordDirName

func stagedInputsDir(agentHome string) string {
	return filepath.Join(agentHome, ".scion", "harness", "inputs")
}

// snapshotControlPlaneInputs records the inputs ProvisionAgent has just
// staged (after resetStagedProvisioning cleared the bundle, so the control
// plane is their only writer) into agentDir/harness-inputs, replacing any
// earlier copy. Only regular files are copied.
func snapshotControlPlaneInputs(agentDir, agentHome string) error {
	dst := filepath.Join(agentDir, controlPlaneInputsDirName)
	if err := os.RemoveAll(dst); err != nil {
		return fmt.Errorf("reset control-plane inputs: %w", err)
	}
	// The record exists (possibly empty) once the control plane has staged
	// inputs, so seedControlPlaneInputsIfAbsent never applies afterwards.
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("create control-plane inputs record: %w", err)
	}
	return copyRegularFiles(stagedInputsDir(agentHome), dst)
}

// legacyInputNames are the control-plane inputs seedControlPlaneInputsIfAbsent
// takes from an agent home staged before the control plane recorded them.
var legacyInputNames = []string{"instructions.md", "system-prompt.md", "resolved-skills.json"}

// seedControlPlaneInputsIfAbsent creates the control plane's record of the
// per-agent inputs for an agent provisioned before that record existed. It
// relies on the record lying outside every container mount scion computes
// (see controlPlaneInputsDirName); author-configured volumes are a separately
// tracked capability, outside this change's scope. Only
// when agentDir/harness-inputs is absent, it copies exactly
// instructions.md, system-prompt.md and resolved-skills.json (fixed names,
// no recursion, no globs) from the agent home's current inputs/ into the
// record, once. Each entry is checked with Lstat and accepted only as a
// regular file (mode&os.ModeType == 0): symlinks and every other non-regular
// type are skipped with a warning and never followed. Accepted files are
// copied by content into new files. A warning names the agent when the
// record is seeded. It reports whether it seeded. Re-provisioning the agent
// replaces the record with freshly staged control-plane inputs.
func seedControlPlaneInputsIfAbsent(agentDir, agentHome, agentID string) (bool, error) {
	record := filepath.Join(agentDir, controlPlaneInputsDirName)
	if _, err := os.Lstat(record); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	// The record is created whether or not anything qualifies, so this runs
	// at most once per agent: an empty record means "nothing to restore",
	// never "absent".
	if err := os.MkdirAll(record, 0o755); err != nil {
		return false, err
	}
	src := stagedInputsDir(agentHome)
	var seeded bool
	for _, name := range legacyInputNames {
		path := filepath.Join(src, name)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		if info.Mode()&os.ModeType != 0 {
			slog.Warn("harness input not seeded: not a regular file", "agent_id", agentID, "file", name, "mode", info.Mode().Type().String())
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return false, err
		}
		if err := os.WriteFile(filepath.Join(record, name), data, 0o644); err != nil {
			return false, err
		}
		seeded = true
	}
	if seeded {
		slog.Warn("harness inputs seeded once from pre-existing inputs; re-provision to refresh", "agent_id", agentID)
	} else {
		slog.Info("harness inputs record created empty: no pre-existing inputs qualified", "agent_id", agentID)
	}
	return seeded, nil
}

// ensureControlPlaneInputsRecord creates an empty inputs record when none
// exists, for an agent whose harness is not container-script, so the agent
// is never later treated as one provisioned before the record existed.
func ensureControlPlaneInputsRecord(agentDir string) error {
	return os.MkdirAll(filepath.Join(agentDir, controlPlaneInputsDirName), 0o755)
}

// resetControlPlaneInputsRecord replaces the inputs record with an empty one
// (ProvisionAgent, for a harness that is not container-script and stages no
// inputs).
func resetControlPlaneInputsRecord(agentDir string) error {
	record := filepath.Join(agentDir, controlPlaneInputsDirName)
	if err := os.RemoveAll(record); err != nil {
		return err
	}
	return os.MkdirAll(record, 0o755)
}

// restoreControlPlaneInputs restages the control plane's copy of the
// per-agent inputs into the agent home's (cleared) inputs/ before a
// container-script harness is provisioned on start, so inputs/ holds only
// control-plane content. Inputs written per launch (auth candidates,
// telemetry, MCP servers) are staged later in Start. With no recorded copy,
// inputs/ stays empty.
func restoreControlPlaneInputs(agentDir, agentHome string) error {
	dst := stagedInputsDir(agentHome)
	if err := os.RemoveAll(dst); err != nil {
		return fmt.Errorf("clear staged inputs: %w", err)
	}
	src := filepath.Join(agentDir, controlPlaneInputsDirName)
	if _, err := os.Stat(src); os.IsNotExist(err) {
		return nil
	}
	return copyRegularFiles(src, dst)
}

// copyRegularFiles copies the regular files directly in src into dst
// (created 0755). A missing src copies nothing.
func copyRegularFiles(src, dst string) error {
	entries, err := os.ReadDir(src)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// stagedSecretsDir is the agent home's staged secrets directory.
func stagedSecretsDir(agentHome string) string {
	return filepath.Join(agentHome, ".scion", "harness", "secrets")
}

// ensureSecretsRecord creates an empty secrets record (mode 0700) in agentDir
// when none exists, leaving an existing record as it is. An empty record
// restores nothing; it is distinct from an absent record.
func ensureSecretsRecord(agentDir string) error {
	record := filepath.Join(agentDir, config.HarnessSecretsRecordDirName)
	if err := os.MkdirAll(record, 0o700); err != nil {
		return err
	}
	return os.Chmod(record, 0o700)
}

// restoreSecretsRecord restores exactly the secret files in the control
// plane's record (agentDir/harness-secrets) into the agent home's cleared
// staged secrets directory (0700, files 0600), and returns their names for
// ContainerScriptHarness.SetRecordedSecrets. Only regular files are restored.
//
// Recorded secrets are restored only for the harness-config revision that
// staged them: nothing is restored unless the record's identity matches
// current, the identity of the harness-config this launch resolved. A record
// without an identity, an unreadable identity, or an unknown current identity
// restores nothing.
//
// With no record (an agent provisioned before harness secrets were recorded),
// nothing is restored, a warning names the agent, and an empty record is
// created so the warning is not repeated.
func restoreSecretsRecord(agentDir, agentHome, agentID string, current *harnessConfigIdentity) ([]string, error) {
	record := filepath.Join(agentDir, config.HarnessSecretsRecordDirName)
	entries, err := os.ReadDir(record)
	if os.IsNotExist(err) {
		slog.Warn("file-type auth secrets are not restored for agents provisioned before harness secrets were recorded; re-create or re-supply credentials", "agent_id", agentID)
		return nil, ensureSecretsRecord(agentDir)
	}
	if err != nil {
		return nil, err
	}
	recorded := readSecretsIdentity(record)
	if !recorded.matches(current) {
		if recorded != nil {
			slog.Info("recorded secrets are restored only for the harness-config revision that staged them; none restored", "agent_id", agentID)
		}
		return nil, nil
	}
	dst := stagedSecretsDir(agentHome)
	var names []string
	for _, e := range entries {
		if !e.Type().IsRegular() || e.Name() == secretsIdentityFile {
			continue
		}
		data, err := os.ReadFile(filepath.Join(record, e.Name()))
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(dst, 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), data, 0o600); err != nil {
			return nil, err
		}
		names = append(names, e.Name())
	}
	return names, nil
}

// recordSecrets replaces the control plane's secrets record with the staged
// secret files ApplyAuthSettings referenced (names), copied by content from
// the agent home's staged secrets directory (regular files only), together
// with current, the identity of the harness-config that staged them (0600).
// The record is written even when names is empty; with no current identity
// no identity is written, so the record restores nothing.
func recordSecrets(agentDir, agentHome string, names []string, current *harnessConfigIdentity) error {
	record := filepath.Join(agentDir, config.HarnessSecretsRecordDirName)
	if err := os.RemoveAll(record); err != nil {
		return err
	}
	if err := ensureSecretsRecord(agentDir); err != nil {
		return err
	}
	src := stagedSecretsDir(agentHome)
	for _, name := range names {
		if name == secretsIdentityFile {
			continue
		}
		info, err := os.Lstat(filepath.Join(src, name))
		if err != nil || info.Mode()&os.ModeType != 0 {
			continue
		}
		data, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(record, name), data, 0o600); err != nil {
			return err
		}
	}
	if current != nil {
		data, err := json.Marshal(current)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(record, secretsIdentityFile), data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// secretsIdentityFile is the file in the secrets record holding the identity
// of the harness-config that staged the recorded secrets. Its name cannot be
// a secret name (secret names are environment-variable names).
const secretsIdentityFile = ".identity.json"

// harnessConfigIdentity identifies the harness-config a launch resolved, as
// computed by the control plane from its own resolution, resolved from
// broker-side agent state only. Recorded secrets are restored only for the
// harness-config revision that staged them.
type harnessConfigIdentity struct {
	Name string `json:"name"`
	// Source is the resolution branch (config.HarnessConfigSource).
	Source string `json:"source"`
	// HubRecordID is the hub harness-config record ID, set only for a
	// hub-hydrated harness-config.
	HubRecordID string `json:"hub_record_id,omitempty"`
	// Revision is the content revision of the resolved harness-config
	// directory (config.ComputeHarnessConfigRevision).
	Revision string `json:"revision"`
}

// currentHarnessConfigIdentity computes the identity of the harness-config
// directory a launch resolved, over the directory the control plane actually
// uses, after resolution. It returns nil when the identity cannot be
// established (no directory, a missing or partly unreadable directory, no
// revision, or a hub-hydrated directory without its record ID); recorded
// secrets are then not restored.
//
// The comparison only restricts restores: it never grants or gates anything
// else. Changing the resolved directory's content changes its revision, so it
// can only make that harness-config's own record fail to match (nothing
// restored); it can never make a different harness-config's record match,
// since name, source and hub record ID are part of the identity.
func currentHarnessConfigIdentity(name string, dir *config.HarnessConfigDir, hubRecordID string) *harnessConfigIdentity {
	if name == "" || dir == nil || dir.Path == "" || !dirFullyReadable(dir.Path) {
		return nil
	}
	rev := config.ComputeHarnessConfigRevision(dir.Path)
	if rev == "" {
		return nil
	}
	id := &harnessConfigIdentity{Name: name, Source: string(dir.Source), Revision: rev}
	if dir.Source == config.HarnessConfigSourceHubHydrated {
		if hubRecordID == "" {
			return nil
		}
		id.HubRecordID = hubRecordID
	}
	return id
}

func (a *harnessConfigIdentity) matches(b *harnessConfigIdentity) bool {
	return a != nil && b != nil && a.Name != "" && a.Revision != "" && *a == *b
}

// readSecretsIdentity reads the identity recorded with the secrets record, or
// nil when absent or unreadable.
func readSecretsIdentity(record string) *harnessConfigIdentity {
	data, err := os.ReadFile(filepath.Join(record, secretsIdentityFile))
	if err != nil {
		return nil
	}
	var id harnessConfigIdentity
	if err := json.Unmarshal(data, &id); err != nil {
		return nil
	}
	return &id
}

// dirFullyReadable reports whether path is a directory whose entries can all
// be walked and whose regular files can all be opened.
func dirFullyReadable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	ok := true
	_ = filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			ok = false
			return filepath.SkipAll
		}
		if d.Type().IsRegular() {
			f, openErr := os.Open(p)
			if openErr != nil {
				ok = false
				return filepath.SkipAll
			}
			_ = f.Close()
		}
		return nil
	})
	return ok
}
