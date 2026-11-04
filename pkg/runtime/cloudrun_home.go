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
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"syscall"

	"github.com/GoogleCloudPlatform/scion/pkg/agenthome"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/stagedsecrets"
)

// A Cloud Run instance's home is always the agent's home leaf on the NFS
// share (agents/<slug>/home-<agentID>), in every homes mode. At each start,
// before the instance is created or started, the broker copies the agent
// home it provisioned (RunConfig.HomeDir) onto that leaf through its own
// host mount of the share, the way a Kubernetes NFS-home pod receives its
// home at each start:
//
//   - The copy leaves out the shared home transfer set's excludes
//     (homeTransferExcludes, shared with Kubernetes) plus the hub credential
//     file and secrets.json (cloudRunHomeExcludes): nothing secret is copied.
//     The hub credential keeps its own path (Staged.HubToken).
//   - The harness bundle's secrets travel in the staged blob
//     (Staged.HarnessSecrets) and sciontool init writes them into an
//     in-memory instance volume (cloudRunMemVolume), never onto the share.
//     The harness outputs are regenerated on the same volume.
//   - After the copy, any harness secrets or outputs left on the home leaf
//     are removed.

// cloudRunHomeMax* bound the home copy. Over a bound, the start is refused.
const (
	cloudRunHomeMaxEntries = 4096
	cloudRunHomeMaxBytes   = 64 << 20
	cloudRunHomeMaxDepth   = 32
)

// cloudRunMemVolume is the in-memory instance volume, mounted at k8sMemDir,
// that holds the harness secrets (k8sHarnessSecretsDir) and the harness
// outputs (k8sHarnessOutputsDir). Its size limit covers both; a write past
// it fails in the instance.
const (
	cloudRunMemVolume          = "scion-mem"
	cloudRunMemVolumeSizeLimit = "16Mi"
)

// The hub credential file (cloudRunHubTokenRel), its staging file and
// secrets.json are never copied onto the share: the credential keeps its
// staged-blob path, and secrets.json is written in the instance from the
// blob. Paths are relative to the home.
const (
	cloudRunHubTokenTmpHomeRel = cloudRunHubTokenRel + ".tmp"
	cloudRunSecretsJSONHomeRel = ".scion/secrets.json"
)

// cloudRunTempNameRE matches the temporary names the broker's guarded
// writers leave behind after a crash: agenthome's ".<name>.tmp-<16 hex>"
// and dirfd's ".<name>.tmp-<pid>-<nanoseconds>". Such a leftover may hold a
// credential being written (the hub token's staging file), so it is never
// copied onto the share.
var cloudRunTempNameRE = regexp.MustCompile(`^\..+\.tmp-([0-9a-f]{16}|[0-9]+-[0-9]+)$`)

// cloudRunHomeCopyExcluded reports whether the home copy leaves out the
// source entry rel (slash-separated, relative to the home).
func cloudRunHomeCopyExcluded(rel string, excludes []string) bool {
	return homeRelExcluded(rel, excludes) || cloudRunTempNameRE.MatchString(path.Base(rel))
}

// CloudRunHomesWarning is the warning a broker logs at start, and scion
// doctor reports, when server.project_storage.homes is set with the
// cloudrun runtime. It is fixed text with no paths.
const CloudRunHomesWarning = "project_storage.homes does not apply to the cloudrun runtime; Cloud Run agent homes are always provisioned onto the share"

// CloudRunHomesWarningFor returns CloudRunHomesWarning when runtimeName is
// the cloudrun runtime and srv sets the homes storage setting (directly or
// through a legacy block), and "" otherwise. A flat cloudrun instance in
// srv (server.broker.instances) requires project_storage.homes: nfs, so the
// setting applies there and no warning is given: the broker never logs it
// and scion doctor never advises removing the setting. The skip is
// settings-wide on purpose: the project storage layout is process-wide, so
// while any flat cloudrun instance is configured the setting applies to the
// whole host, and the warning (with its advice to remove the setting) would
// be wrong for a legacy cloudrun runtime entry too. The warning is only ever
// given for the cloudrun runtime, so no docker or kubernetes caller is
// affected.
func CloudRunHomesWarningFor(runtimeName string, srv *config.V1ServerConfig) string {
	if runtimeName != "cloudrun" || srv == nil || hasFlatCloudRunInstance(srv) {
		return ""
	}
	ps, _, err := config.ResolveProjectStorage(srv)
	if err != nil || ps == nil || ps.Homes == "" {
		return ""
	}
	return CloudRunHomesWarning
}

// hasFlatCloudRunInstance reports whether srv declares a flat Runtime
// Broker instance whose runtime target type is cloudrun.
func hasFlatCloudRunInstance(srv *config.V1ServerConfig) bool {
	if srv == nil || srv.Broker == nil {
		return false
	}
	for _, inst := range srv.Broker.Instances {
		if inst.RuntimeTarget != nil && inst.RuntimeTarget.Type == config.RuntimeTargetTypeCloudRun {
			return true
		}
	}
	return false
}

// cloudRunHomeExcludes returns what the Cloud Run home copy leaves out,
// relative to the home: the shared home transfer set's excludes
// (homeTransferExcludes, with secrets.json delivered), then the hub
// credential file, its staging file and secrets.json whether or not they
// are staged. Temporary leftovers are left out too (cloudRunTempNameRE).
func cloudRunHomeExcludes(cfg RunConfig) []string {
	return append(homeTransferExcludes(cfg, true), cloudRunHubTokenRel, cloudRunHubTokenTmpHomeRel, cloudRunSecretsJSONHomeRel)
}

// Fixed errors of the home copy. They name no path, secret or value.
var (
	errCloudRunHomeTooLarge = fmt.Errorf("cloudrun: the agent home exceeds the limits for provisioning it onto the share (%d entries, %d MiB, %d directory levels); the start is refused",
		cloudRunHomeMaxEntries, cloudRunHomeMaxBytes>>20, cloudRunHomeMaxDepth)
	errCloudRunHomeConflict = errors.New("cloudrun: the agent home on the share has an entry of another type where the provisioned home has a file or directory; the start is refused")
	errCloudRunHomeFailed   = errors.New("cloudrun: the agent home could not be provisioned onto the share; the start is refused")
)

// cloudRunHomeRemovals are removed from the home leaf after each copy, so
// no harness secrets or outputs stay at rest on the share.
var cloudRunHomeRemovals = []string{harnessBundleSecretsRel, harnessBundleOutputsRel}

// provisionCloudRunHome copies cfg.HomeDir onto the agent's home leaf on
// the share and then removes cloudRunHomeRemovals from the leaf.
//
// The leaf is opened from the host base with each component below it
// opened without following a link (agenthome.OpenBelow). The copy is
// agenthome.SeedTree: an fd-relative, no-follow walk on both sides; new
// directories 0700 and every file written through a fresh O_EXCL|O_NOFOLLOW
// temporary file, fchowned to uid:gid, mode capped (0755 for executables,
// 0644 otherwise, no setuid, setgid or sticky), then renamed into place.
// Files on the leaf that the copy does not write are kept. A link on the
// leaf where a file or directory goes is replaced, never followed or
// written through, and its target is never touched; replacements are
// logged as a count. Any other entry of the wrong type refuses the start
// and is logged by home-relative name. Links, special files and
// hard-linked files in the source are skipped and logged by home-relative
// name. The removals never follow a link (agenthome.Home.RemoveAll).
//
// A failure part-way refuses the start; entries already renamed into
// place are complete files, and the next start copies again. Logs give
// counts, and home-relative names only for skipped entries and conflicts.
func provisionCloudRunHome(cfg RunConfig, paths *cloudRunNFSProvisionPaths, uid, gid int) error {
	agentID := cfg.Labels["agent_id"]
	home, err := ValidateAgentHomeSource(cfg.HomeDir)
	if err != nil || home == "" {
		runtimeLog().Warn("Cloud Run start: the provisioned agent home is not an allowed home path", "agent_id", agentID)
		return errCloudRunHomeFailed
	}
	src, err := os.OpenRoot(home)
	if err != nil {
		runtimeLog().Warn("Cloud Run start: cannot open the provisioned agent home", "agent_id", agentID, "error_class", errClass(err))
		return errCloudRunHomeFailed
	}
	defer func() { _ = src.Close() }()
	leaf, err := agenthome.OpenBelow(paths.hostBase, paths.homeRel)
	if err != nil {
		return cloudRunHomeErr(agentID, "open the home leaf", err)
	}
	defer func() { _ = leaf.Close() }()

	excludes := cloudRunHomeExcludes(cfg)
	res, err := leaf.SeedTree(src, agenthome.SeedOptions{
		Exclude: func(rel string) bool { return cloudRunHomeCopyExcluded(rel, excludes) },
		UID:     uid,
		GID:     gid,
		Limits: agenthome.Limits{
			MaxFiles: cloudRunHomeMaxEntries,
			MaxBytes: cloudRunHomeMaxBytes,
			MaxDepth: cloudRunHomeMaxDepth,
		},
	})
	for _, rel := range res.Skipped {
		runtimeLog().Warn("Cloud Run start: skipped a link, special file or hard-linked file in the agent home", "agent_id", agentID, "entry", rel)
	}
	if err != nil {
		return cloudRunHomeErr(agentID, "copy", err)
	}
	for _, rel := range cloudRunHomeRemovals {
		if err := leaf.RemoveAll(rel); err != nil {
			return cloudRunHomeErr(agentID, "remove harness material from the home leaf", err)
		}
	}
	runtimeLog().Info("Cloud Run start: agent home provisioned onto the share",
		"agent_id", agentID, "dirs", res.Dirs, "files", res.Files, "bytes", res.Bytes,
		"skipped", len(res.Skipped), "replaced_links", len(res.Replaced))
	return nil
}

// cloudRunHomeErr logs a home copy failure without host paths and returns
// the fixed error for its class.
func cloudRunHomeErr(agentID, step string, err error) error {
	var limit *agenthome.LimitError
	var conflict *agenthome.SeedConflictError
	switch {
	case errors.As(err, &limit):
		runtimeLog().Warn("Cloud Run start: the agent home exceeds the copy limits", "agent_id", agentID, "limit", limit.Limit)
		return errCloudRunHomeTooLarge
	case errors.As(err, &conflict):
		runtimeLog().Warn("Cloud Run start: refused a wrong-type entry on the home leaf", "agent_id", agentID, "entry", conflict.Rel, "reason", conflict.Reason)
		return errCloudRunHomeConflict
	case errors.Is(err, agenthome.ErrLinkComponent), errors.Is(err, agenthome.ErrNotDirectory):
		runtimeLog().Warn("Cloud Run start: refused a symbolic link or wrong-type directory on the home leaf", "agent_id", agentID, "step", step)
		return errCloudRunHomeConflict
	default:
		runtimeLog().Warn("Cloud Run start: provisioning the agent home onto the share failed", "agent_id", agentID, "step", step, "error_class", errClass(err))
		return errCloudRunHomeFailed
	}
}

// errClass returns the system error text of err (for example "permission
// denied") without the path an *fs.PathError carries, or "other".
func errClass(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno.Error()
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "not found"
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	}
	return "other"
}

// cloudRunMaxHarnessSecretBytes bounds one harness secret file read from
// the agent home. Any larger file cannot fit in the staged blob anyway.
const cloudRunMaxHarnessSecretBytes = cloudRunMaxEnvValueBytes

// Conditions the harness secrets reader logs. The reader also logs which
// of its three fixed components (.scion, .scion/harness,
// .scion/harness/secrets) applies, never a host path or a secret name.
const (
	harnessSecretsCondLinked       = "linked"
	harnessSecretsCondNotDirectory = "not_directory"
)

// errCloudRunHarnessSecrets is the fixed error for harness secrets that
// cannot be read from the provisioned agent home.
var errCloudRunHarnessSecrets = errors.New("cloudrun: the harness secrets could not be read from the agent home; the start is refused")

// readCloudRunHarnessSecrets returns the harness bundle's staged secret
// files from the provisioned agent home (homeDir/.scion/harness/secrets),
// for the staged blob. Each directory component is checked with Lstat
// inside an os.Root on the home and never followed:
//   - a missing home or directory, or a home path that
//     ValidateAgentHomeSource refuses, gives none;
//   - a link at any component, or a non-directory at .scion/harness/secrets
//     itself, gives none with a warning naming the condition (linked or
//     not_directory) and the fixed component; the start continues, as for
//     the hub credential;
//   - a non-directory at .scion or .scion/harness refuses the start with
//     errCloudRunHomeConflict, since the provisioned home needs those
//     directories.
//
// Only top-level regular files with a plain name are read: that
// is the contract of the directory, which pkg/agent fills with flat 0600
// files and the harness reads non-recursively (readStagedSecretDir), and
// sciontool init writes them back flat at 0600. A file replaced since it was
// listed is refused. Other entries (subdirectories, links, special files) are
// skipped and counted. Errors and logs name no value and no file name.
func readCloudRunHarnessSecrets(homeDir, agentID string) ([]stagedsecrets.HarnessSecret, error) {
	if homeDir == "" {
		return nil, nil
	}
	// A home path that is not allowed stages nothing, as for the hub
	// credential (readCloudRunHubToken); the home copy then refuses the
	// start.
	home, err := ValidateAgentHomeSource(homeDir)
	if err != nil || home == "" {
		runtimeLog().Warn("Cloud Run start: the harness secrets are not staged: the agent home is not an allowed home path", "agent_id", agentID)
		return nil, nil
	}
	root, err := os.OpenRoot(home)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		runtimeLog().Warn("Cloud Run start: cannot open the provisioned agent home", "agent_id", agentID, "error_class", errClass(err))
		return nil, errCloudRunHarnessSecrets
	}
	defer func() { _ = root.Close() }()
	cur := ""
	for _, part := range []string{".scion", "harness", "secrets"} {
		cur = path.Join(cur, part)
		fi, err := root.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			runtimeLog().Warn("Cloud Run start: cannot read the harness secrets directory in the agent home", "agent_id", agentID, "component", cur, "error_class", errClass(err))
			return nil, errCloudRunHarnessSecrets
		}
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			// A link is never followed or read: nothing is staged and the
			// start continues, as for the hub credential
			// (readCloudRunHubToken). The home copy skips a source link too.
			runtimeLog().Warn("Cloud Run start: the harness secrets are not staged", "agent_id", agentID, "condition", harnessSecretsCondLinked, "component", cur)
			return nil, nil
		case !fi.IsDir() && part == "secrets":
			// A non-directory at the secrets path itself: it is excluded
			// from the home copy, so nothing is staged and the start
			// continues.
			runtimeLog().Warn("Cloud Run start: the harness secrets are not staged", "agent_id", agentID, "condition", harnessSecretsCondNotDirectory, "component", cur)
			return nil, nil
		case !fi.IsDir():
			// A non-directory at .scion or .scion/harness cannot hold the
			// provisioned home's directories: the start is refused.
			runtimeLog().Warn("Cloud Run start: a parent of the harness secrets directory in the agent home is not a directory; the start is refused", "agent_id", agentID, "condition", harnessSecretsCondNotDirectory, "component", cur)
			return nil, errCloudRunHomeConflict
		}
	}
	dir, err := root.Open(harnessBundleSecretsRel)
	if err != nil {
		return nil, errCloudRunHarnessSecrets
	}
	entries, err := dir.ReadDir(-1)
	_ = dir.Close()
	if err != nil {
		return nil, errCloudRunHarnessSecrets
	}
	var out []stagedsecrets.HarnessSecret
	skipped := 0
	for _, e := range entries {
		name := e.Name()
		rel := path.Join(harnessBundleSecretsRel, name)
		fi, err := root.Lstat(rel)
		if err != nil {
			return nil, errCloudRunHarnessSecrets
		}
		if !fi.Mode().IsRegular() || !stagedsecrets.ValidHarnessSecretName(name) {
			skipped++
			continue
		}
		data, err := readRegularInRoot(root, rel, fi, cloudRunMaxHarnessSecretBytes)
		if errors.Is(err, errHarnessSecretTooLarge) {
			return nil, fmt.Errorf("cloudrun: staged secrets do not fit: one harness secret is over the %d-byte limit for one environment variable value on this runtime; the largest part is the %s",
				cloudRunMaxEnvValueBytes, stagedClassHarnessSecrets)
		}
		if err != nil {
			runtimeLog().Warn("Cloud Run start: cannot read a harness secret from the agent home", "agent_id", agentID, "error_class", errClass(err))
			return nil, errCloudRunHarnessSecrets
		}
		out = append(out, stagedsecrets.HarnessSecret{Name: name, Value: base64.StdEncoding.EncodeToString(data)})
	}
	if skipped > 0 {
		runtimeLog().Warn("Cloud Run start: skipped entries in the harness secrets directory that are not plain files", "agent_id", agentID, "count", skipped)
	}
	return out, nil
}

// errHarnessSecretTooLarge reports a harness secret file over the read bound.
var errHarnessSecretTooLarge = errors.New("file too large")

// readRegularInRoot reads the regular file rel in root (fi is its Lstat),
// up to max bytes, refusing a file replaced since the Lstat.
func readRegularInRoot(root *os.Root, rel string, fi fs.FileInfo, max int64) ([]byte, error) {
	f, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || !os.SameFile(fi, st) {
		return nil, errors.New("changed while being read")
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, errHarnessSecretTooLarge
	}
	return data, nil
}
