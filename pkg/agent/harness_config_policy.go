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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/agenthome"
	"github.com/GoogleCloudPlatform/scion/pkg/agentplugin"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
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
	if err := writeAgentHome(ctx, func() error { return resetStagedProvisioning(agentHome) }); err != nil {
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

// stageFallbackCaptureAuthAssets stages capture-auth assets (capture_auth.py +
// capture-auth-config.json) for a non-container-script harness when permitted
// by the context's harness-config policy (ptone/scion#622). Container-script
// harnesses stage these inside (*ContainerScriptHarness).Provision.
//
// Because capture_auth.py is an executable script from the harness-config
// bundle (staged 0755 at .scion/harness/capture_auth.py), staging capture-auth
// assets on the non-container-script path is subject to the same context
// policy gate that blocks container-script harnesses
// (AllowContainerScriptHarnesses):
//   - when no policy is attached (AllowContainerScriptHarnesses=true or solo
//     CLI), staging runs as before;
//   - when a policy is attached and capture-auth would stage any asset
//     (capture_auth.py is present, statting it fails with a non-ENOENT error so
//     an uncertain policy check fails closed, or auth metadata declares
//     capture-auth required_files), the policy is probed with a
//     container-script Provisioner on the entry; if the policy refuses,
//     neither capture_auth.py nor capture-auth-config.json is staged, while
//     the declarative harness launch itself proceeds;
//   - in the allowed case, staging I/O errors warn on stderr without failing
//     the launch.
func stageFallbackCaptureAuthAssets(
	ctx context.Context,
	agentName string,
	h api.Harness,
	harnessConfigName string,
	hcDir *config.HarnessConfigDir,
	agentHome string,
	writeHome func(func() error) error,
) {
	if _, isContainerScript := h.(*harness.ContainerScriptHarness); isContainerScript || hcDir == nil || hcDir.Path == "" {
		return
	}
	if policy := config.HarnessConfigPolicyFromContext(ctx); policy != nil {
		_, statErr := os.Lstat(filepath.Join(hcDir.Path, "capture_auth.py"))
		if statErr == nil || !errors.Is(statErr, os.ErrNotExist) || hasCaptureAuthCredentials(hcDir.Config.Auth) {
			probe := hcDir.Config
			probe.Provisioner = &config.HarnessProvisionerConfig{Type: "container-script"}
			if err := policy(harnessConfigName, probe); err != nil {
				if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
					fmt.Fprintf(os.Stderr, "Warning: capture-auth asset check failed; skipping staging under policy: %v\n", statErr)
				}
				harnessName := ""
				if h != nil {
					harnessName = h.Name()
				}
				slog.Info("capture-auth asset staging suppressed",
					"agent", agentName,
					"harness", harnessName,
					"harness_config", harnessConfigName,
					"reason", "container-script harnesses disallowed",
				)
				return
			}
		}
	}
	stage := func() error {
		return harness.StageCaptureAuthAssets(agentHome, hcDir.Path, hcDir.Config.Auth)
	}
	var err error
	if writeHome != nil {
		err = writeHome(stage)
	} else {
		err = stage()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: capture-auth asset staging failed: %v\n", err)
	}
}

func hasCaptureAuthCredentials(authMeta *config.HarnessAuthMetadata) bool {
	if authMeta == nil {
		return false
	}
	for _, authType := range authMeta.Types {
		for _, rf := range authType.RequiredFiles {
			if rf.Name != "" && rf.TargetSuffix != "" {
				return true
			}
		}
	}
	return false
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
// earlier copy. Only regular files are copied: the top-level files of
// inputs/, plus the staged Agent Plugins tree inputs/plugins/ (copied with
// agentplugin.CopyTree, which never follows a symlink).
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
	if err := copyRegularFiles(stagedInputsDir(agentHome), dst); err != nil {
		return err
	}
	stagedPlugins := filepath.Join(agentplugin.InputsRel, agentplugin.PluginsDirName)
	return copyPluginsTree(agentHome, stagedPlugins, dst, agentplugin.PluginsDirName)
}

// copyPluginsTree copies the staged Agent Plugins tree srcRel under srcDir
// (one directory per plugin) to dstRel under dstDir, replacing dstRel. Each
// plugin directory is copied with its own agentplugin.CopyTree and the
// per-plugin agentplugin.DefaultLimits, the same bound Stage applied to that
// bundle. A missing source, or one that is not a real directory, copies
// nothing; entries that are not real directories with a valid plugin name
// are skipped.
func copyPluginsTree(srcDir, srcRel, dstDir, dstRel string) error {
	return copyPluginsTreeWithLimits(srcDir, srcRel, dstDir, dstRel, agentplugin.DefaultLimits)
}

func copyPluginsTreeWithLimits(srcDir, srcRel, dstDir, dstRel string, lim agentplugin.Limits) error {
	var dstRoot *os.Root
	defer func() {
		if dstRoot != nil {
			_ = dstRoot.Close()
		}
	}()
	reset := func() error {
		var err error
		if dstRoot, err = os.OpenRoot(dstDir); err != nil {
			return err
		}
		if err := dstRoot.RemoveAll(dstRel); err != nil {
			return err
		}
		return dstRoot.MkdirAll(dstRel, 0o755)
	}
	install := func(srcRoot *os.Root, srcPlugin, name string) error {
		_, err := agentplugin.CopyTree(srcRoot, srcPlugin, dstRoot, filepath.Join(dstRel, name), lim)
		return err
	}
	return walkPluginsTree(srcDir, srcRel, reset, install)
}

// walkPluginsTree is the plugin-directory walk shared by both directions of
// the Agent Plugins tree copy (snapshot: home to record, copyPluginsTree;
// restore: record to home, restorePluginsTree). A missing source, or one
// that is not a real directory, copies nothing. Otherwise reset prepares
// the destination, and install copies each entry of srcRel that is a real
// directory with a valid plugin name (srcPlugin is its path in srcRoot),
// in name order; an install error names the plugin.
func walkPluginsTree(srcDir, srcRel string, reset func() error, install func(srcRoot *os.Root, srcPlugin, name string) error) error {
	srcRoot, err := os.OpenRoot(srcDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer func() { _ = srcRoot.Close() }()
	srcRel = filepath.Clean(srcRel)
	if ok, err := realDirChain(srcRoot, srcRel, true); err != nil || !ok {
		return err
	}
	if err := reset(); err != nil {
		return err
	}
	entries, err := readRootDir(srcRoot, srcRel)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if agentplugin.ValidateName(name) != nil {
			continue
		}
		fi, err := srcRoot.Lstat(filepath.Join(srcRel, name))
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			continue
		}
		if err := install(srcRoot, filepath.Join(srcRel, name), name); err != nil {
			return fmt.Errorf("copy agent plugin %q: %w", name, err)
		}
	}
	return nil
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
//
// includePlugins (hub.agent_plugins on for this start) also restores the
// recorded Agent Plugins tree inputs/plugins/. Without it plugins.json is
// removed too, so the container sees no desired plugins and provision.py
// removes the plugins Scion installed earlier.
//
// Every removal and write is confined to the agent home (pkg/agenthome),
// through one handle opened on the home: inputs/ is cleared without
// following a link, files are installed by a temporary file renamed over
// the leaf (a link there is replaced, never written through), and a linked
// directory component is refused.
func restoreControlPlaneInputs(agentDir, agentHome string, includePlugins bool) error {
	home, err := agenthome.OpenCreate(agentHome, 0o755)
	if err != nil {
		return err
	}
	defer func() { _ = home.Close() }()
	if err := home.RemoveAll(agentplugin.InputsRel); err != nil {
		return fmt.Errorf("clear staged inputs: %w", err)
	}
	if restoreInputsTestHook != nil {
		restoreInputsTestHook("cleared")
	}
	src := filepath.Join(agentDir, controlPlaneInputsDirName)
	if _, err := os.Stat(src); os.IsNotExist(err) {
		return nil
	}
	if err := copyRegularFilesToHome(src, home, agentplugin.InputsRel); err != nil {
		return err
	}
	if restoreInputsTestHook != nil {
		restoreInputsTestHook("files")
	}
	if !includePlugins {
		return home.Remove(path.Join(agentplugin.InputsRel, agentplugin.IndexFileName))
	}
	return restorePluginsTree(src, agentplugin.PluginsDirName, home, path.Join(agentplugin.InputsRel, agentplugin.PluginsDirName))
}

// restoreInputsTestHook, when non-nil, is called by
// restoreControlPlaneInputs after inputs/ is cleared ("cleared") and after
// the top-level files are restored ("files"), so a test can change the home
// between those operations. Always nil in production.
var restoreInputsTestHook func(stage string)

// copyRegularFilesToHome copies the regular files directly in src (a
// control-plane record) into dstRel in the agent home (created 0755),
// confined to the home. A missing src copies nothing.
func copyRegularFilesToHome(src string, home *agenthome.Home, dstRel string) error {
	entries, err := os.ReadDir(src)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := home.MkdirAll(dstRel, 0o755); err != nil {
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
		if err := home.WriteFile(path.Join(dstRel, e.Name()), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// restorePluginsTree restores the recorded Agent Plugins tree srcRel under
// srcDir (one directory per plugin) to dstRel in the agent home, replacing
// dstRel, confined to the home. Each plugin directory is installed with
// agenthome.InstallTree under the per-plugin agentplugin.DefaultLimits. A
// missing source, or one that is not a real directory, copies nothing;
// entries that are not real directories with a valid plugin name are
// skipped.
func restorePluginsTree(srcDir, srcRel string, home *agenthome.Home, dstRel string) error {
	return restorePluginsTreeWithLimits(srcDir, srcRel, home, dstRel, agentplugin.DefaultLimits)
}

func restorePluginsTreeWithLimits(srcDir, srcRel string, home *agenthome.Home, dstRel string, lim agentplugin.Limits) error {
	reset := func() error {
		if err := home.RemoveAll(dstRel); err != nil {
			return err
		}
		return home.MkdirAll(dstRel, 0o755)
	}
	homeLim := agenthome.Limits{MaxFiles: lim.MaxFiles, MaxBytes: lim.MaxBytes, MaxDepth: lim.MaxDepth}
	install := func(srcRoot *os.Root, srcPlugin, name string) error {
		err := home.InstallTree(srcRoot, srcPlugin, path.Join(dstRel, name), homeLim)
		var le *agenthome.LimitError
		if errors.As(err, &le) {
			// Report a limit the way the snapshot direction does.
			return &agentplugin.LimitError{Limit: le.Limit}
		}
		return err
	}
	return walkPluginsTree(srcDir, srcRel, reset, install)
}

// recordHasAgentPlugins reports whether the control-plane inputs record
// holds a staged plugin index.
func recordHasAgentPlugins(agentDir string) bool {
	fi, err := os.Lstat(filepath.Join(agentDir, controlPlaneInputsDirName, agentplugin.IndexFileName))
	return err == nil && fi.Mode().IsRegular()
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

// stagedSecretsRel is the agent home's staged secrets directory, relative
// to the home.
const stagedSecretsRel = ".scion/harness/secrets"

// stagedSecretsDir is the agent home's staged secrets directory.
func stagedSecretsDir(agentHome string) string {
	return filepath.Join(agentHome, filepath.FromSlash(stagedSecretsRel))
}

const (
	secretsRecordOldDirName = config.HarnessSecretsRecordDirName + ".old"
	secretsRecordTmpPrefix  = config.HarnessSecretsRecordDirName + ".tmp-"
)

// ensureSecretsRecord creates an empty private secrets record (0700 on Unix,
// owner-only DACL on Windows) in agentDir when none exists, after recovering
// any harness-secrets.old left by an interrupted swap. An empty record restores
// nothing and is distinct from an absent record. An existing record directory,
// including its permissions, is left unchanged (privatefs.Tree.MkdirAll does
// not re-chmod existing directories; every recordSecrets replacement installs a
// fresh private directory via rename).
func ensureSecretsRecord(agentDir string) error {
	if err := recoverSecretsRecordOld(agentDir); err != nil {
		return err
	}
	tree, err := privatefs.OpenTree(agentDir, privatefs.TreeOptions{CreateAnchor: true})
	if err != nil {
		return err
	}
	defer func() { _ = tree.Close() }()
	return tree.MkdirAll(config.HarnessSecretsRecordDirName, privatefs.Private)
}

// recoverSecretsRecordOld renames harness-secrets.old back to harness-secrets
// when an atomic swap was interrupted between its two renames.
func recoverSecretsRecordOld(agentDir string) error {
	if _, err := os.Lstat(filepath.Join(agentDir, config.HarnessSecretsRecordDirName)); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := os.Lstat(filepath.Join(agentDir, secretsRecordOldDirName)); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	tree, err := privatefs.OpenTree(agentDir, privatefs.TreeOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tree.Close() }()
	return tree.Rename(secretsRecordOldDirName, config.HarnessSecretsRecordDirName)
}

// cleanupSecretsRecordTmp removes leftover harness-secrets.tmp-* directories in
// agentDir from an earlier crashed recordSecrets call. Callers must ensure no
// concurrent recordSecrets write for the same agent can be in flight.
func cleanupSecretsRecordTmp(agentDir string) error {
	fi, err := os.Lstat(agentDir)
	if os.IsNotExist(err) || (err == nil && !fi.IsDir()) {
		return nil
	}
	if err != nil {
		return err
	}
	matches, err := filepath.Glob(filepath.Join(agentDir, secretsRecordTmpPrefix+"*"))
	if err != nil {
		return err
	}
	for _, m := range matches {
		if err := os.RemoveAll(m); err != nil {
			return err
		}
	}
	return nil
}

var restoreSecretsAllowTestHook func(name string) bool

// restoreSecretsRecord restores recorded regular secret files from
// agentDir/harness-secrets into the agent home's cleared staged secrets
// directory (0700, files 0600), and returns their names for
// ContainerScriptHarness.SetRecordedSecrets. Any harness-secrets.old left by an
// interrupted recordSecrets swap is recovered first.
//
// Exact identity matches restore every recorded regular file; same-lineage
// revision changes (matching name, source, and hub record ID) with a non-nil
// allow predicate restore only accepted names without writing filtered files to
// the home; any other mismatch, missing or unreadable identity, or unknown
// current identity restores nothing.
//
// With no record (an agent provisioned before harness secrets were recorded),
// nothing is restored, a warning names the agent, and an empty record is
// created so the warning is not repeated.
//
// Writes into the home are confined via pkg/agenthome through one handle opened
// on the home: a link at a secret file is replaced, never written through, and
// a linked directory component is refused.
func restoreSecretsRecord(agentDir, agentHome, agentID string, current *harnessConfigIdentity, allow func(name string) bool) ([]string, error) {
	if err := recoverSecretsRecordOld(agentDir); err != nil {
		return nil, err
	}
	if restoreSecretsAllowTestHook != nil {
		allow = restoreSecretsAllowTestHook
	}
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
	exactMatch := recorded.matches(current)
	crossRevision := !exactMatch && recorded.sameLineage(current) && allow != nil
	if !exactMatch && !crossRevision {
		if recorded != nil {
			slog.Info("recorded secrets are restored only for the harness-config revision that staged them; none restored", "agent_id", agentID)
		}
		return nil, nil
	}
	var home *agenthome.Home
	defer func() {
		if home != nil {
			_ = home.Close()
		}
	}()
	var names, dropped []string
	for _, e := range entries {
		if !e.Type().IsRegular() || e.Name() == secretsIdentityFile {
			continue
		}
		if crossRevision && !allow(e.Name()) {
			dropped = append(dropped, e.Name())
			continue
		}
		data, err := os.ReadFile(filepath.Join(record, e.Name()))
		if err != nil {
			return nil, err
		}
		if home == nil {
			if home, err = agenthome.OpenCreate(agentHome, 0o700); err != nil {
				return nil, err
			}
			if err := home.MkdirAll(stagedSecretsRel, 0o700); err != nil {
				return nil, err
			}
		}
		if err := home.WriteFile(path.Join(stagedSecretsRel, e.Name()), data, 0o600); err != nil {
			return nil, err
		}
		names = append(names, e.Name())
	}
	if crossRevision {
		slog.Info("restoring recorded secrets across a harness-config revision change",
			"agent_id", agentID, "from_revision", recorded.Revision, "to_revision", current.Revision,
			"restored", names, "dropped", dropped)
	}
	return names, nil
}

type recordUpdate struct {
	Names          []string
	Current        *harnessConfigIdentity
	FreshSatisfied bool
	Allow          func(name string) bool
}

var recordSecretsBetweenRenamesHook func() error

// recordSecrets updates agentDir/harness-secrets from the regular secret files
// referenced in u.Names (copied by content from the agent home's staged secrets
// directory) together with u.Current (written last as .identity.json). It
// returns replaced=true when a new record was installed, or replaced=false with
// a nil error when an existing same-lineage record was preserved because
// !u.FreshSatisfied and replacing would drop a recorded key still valid for the
// target revision.
//
// At entry, any harness-secrets.old left by an interrupted swap is recovered
// and any leftover harness-secrets.tmp-* directories from a prior crash are
// removed. Replacements write a private harness-secrets.tmp-<hex> directory,
// rename an existing harness-secrets aside to harness-secrets.old, rename the
// populated temp directory into place, and remove .old; on a crash at any
// point, recovery sees either the old or the new complete record.
func recordSecrets(agentDir, agentHome, agentID string, u recordUpdate) (bool, error) {
	if err := recoverSecretsRecordOld(agentDir); err != nil {
		return false, err
	}
	if err := cleanupSecretsRecordTmp(agentDir); err != nil {
		return false, err
	}
	record := filepath.Join(agentDir, config.HarnessSecretsRecordDirName)
	oldEntries, err := os.ReadDir(record)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	recordExists := err == nil
	var oldID *harnessConfigIdentity
	var oldNames []string
	if recordExists {
		oldID = readSecretsIdentity(record)
		for _, e := range oldEntries {
			if e.Type().IsRegular() && e.Name() != secretsIdentityFile {
				oldNames = append(oldNames, e.Name())
			}
		}
	}

	type stagedSecret struct {
		name string
		data []byte
	}
	newSet := make(map[string]struct{}, len(u.Names))
	var staged []stagedSecret
	src := stagedSecretsDir(agentHome)
	for _, name := range u.Names {
		if name == secretsIdentityFile {
			continue
		}
		if _, seen := newSet[name]; seen {
			continue
		}
		p := filepath.Join(src, name)
		info, err := os.Lstat(p)
		if err != nil || info.Mode()&os.ModeType != 0 {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return false, err
		}
		newSet[name] = struct{}{}
		staged = append(staged, stagedSecret{name: name, data: data})
	}

	replace := oldID == nil || (u.Current != nil && !oldID.sameLineage(u.Current)) || u.FreshSatisfied
	if !replace {
		var lost []string
		for _, n := range oldNames {
			if _, ok := newSet[n]; ok {
				continue
			}
			if u.Current == nil || oldID.matches(u.Current) || u.Allow == nil || u.Allow(n) {
				lost = append(lost, n)
			}
		}
		if len(lost) > 0 {
			slog.Warn("kept the existing harness secrets record: this start staged fewer credentials than recorded for the same harness-config",
				"agent_id", agentID,
				"missing", lost,
			)
			return false, nil
		}
	}

	tree, err := privatefs.OpenTree(agentDir, privatefs.TreeOptions{CreateAnchor: true})
	if err != nil {
		return false, err
	}
	defer func() { _ = tree.Close() }()

	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return false, err
	}
	tmpRel := secretsRecordTmpPrefix + hex.EncodeToString(rnd[:])
	tmpMoved := false
	defer func() {
		if !tmpMoved {
			_ = os.RemoveAll(filepath.Join(agentDir, tmpRel))
		}
	}()
	if err := tree.MkdirAll(tmpRel, privatefs.Private); err != nil {
		return false, err
	}
	for _, s := range staged {
		if err := tree.WriteFile(path.Join(tmpRel, s.name), s.data, privatefs.Private); err != nil {
			return false, err
		}
	}
	if u.Current != nil {
		idData, err := json.Marshal(u.Current)
		if err != nil {
			return false, err
		}
		if err := tree.WriteFile(path.Join(tmpRel, secretsIdentityFile), idData, privatefs.Private); err != nil {
			return false, err
		}
	}

	oldPath := filepath.Join(agentDir, secretsRecordOldDirName)
	if err := os.RemoveAll(oldPath); err != nil {
		return false, err
	}
	if recordExists {
		if err := tree.Rename(config.HarnessSecretsRecordDirName, secretsRecordOldDirName); err != nil {
			return false, err
		}
	}
	if recordSecretsBetweenRenamesHook != nil {
		if err := recordSecretsBetweenRenamesHook(); err != nil {
			return false, err
		}
	}
	if err := tree.Rename(tmpRel, config.HarnessSecretsRecordDirName); err != nil {
		if recordExists {
			_ = tree.Rename(secretsRecordOldDirName, config.HarnessSecretsRecordDirName)
		}
		return false, err
	}
	tmpMoved = true
	_ = os.RemoveAll(oldPath)
	return true, nil
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

func (a *harnessConfigIdentity) sameLineage(b *harnessConfigIdentity) bool {
	return a != nil && b != nil &&
		a.Name != "" && a.Revision != "" && b.Revision != "" &&
		a.Name == b.Name && a.Source == b.Source && a.HubRecordID == b.HubRecordID
}

// readSecretsIdentity reads the identity recorded with the secrets record, or
// nil when absent, unreadable, or incomplete.
func readSecretsIdentity(record string) *harnessConfigIdentity {
	data, err := os.ReadFile(filepath.Join(record, secretsIdentityFile))
	if err != nil {
		return nil
	}
	var id harnessConfigIdentity
	if err := json.Unmarshal(data, &id); err != nil || id.Name == "" || id.Revision == "" {
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
