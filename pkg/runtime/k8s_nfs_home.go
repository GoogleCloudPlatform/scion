package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// NFS-home pods.
//
// An NFS-home pod keeps the agent home on an NFS export instead of in the
// pod's own filesystem. The staged secret and auth-file volumes stay under
// k8sFileStagingRoot (see k8s_file_placement.go), and the files are found
// in the home through symbolic links instead of copies, so no secret
// content is written to the export. Files that must not be written to the
// export at all (harness outputs and harness staged secrets) live in a
// memory-backed directory, k8sMemDir, and are found through env vars.
//
// A pod is an NFS-home pod when RunConfig.HomeStorageBackend is
// HomeStorageNFS; buildPod then stamps k8sHomeStorageAnnotation on it. With
// any other value the pod spec is unchanged.

// HomeStorageNFS is the RunConfig.HomeStorageBackend value for NFS-home pods.
const HomeStorageNFS = "nfs"

// k8sHomeStorageAnnotation marks NFS-home pods. buildPod sets it from
// RunConfig.HomeStorageBackend only; a caller-supplied value is dropped.
const k8sHomeStorageAnnotation = "scion.home_storage"

const (
	// k8sMemVolume is the memory-backed emptyDir of NFS-home pods.
	k8sMemVolume = "scion-mem"
	// k8sMemDir is where k8sMemVolume is mounted.
	k8sMemDir = k8sFileStagingRoot + "/mem"
	// k8sHarnessOutputsDir and k8sHarnessSecretsDir replace the harness
	// bundle's outputs/ and secrets/ directories in NFS-home pods.
	k8sHarnessOutputsDir = k8sMemDir + "/outputs"
	k8sHarnessSecretsDir = k8sMemDir + "/harness-secrets"
	// k8sHomeLinksResultFile records what the link placement step did
	// with each requested link (see k8sHomeLinksResult).
	k8sHomeLinksResultFile = k8sMemDir + "/home-links-result.json"
)

// Env vars set on NFS-home pods only.
const (
	homeLinksEnvVar         = "SCION_HOME_LINKS"
	secretsFileEnvVar       = "SCION_SECRETS_FILE"
	harnessOutputsDirEnvVar = "SCION_HARNESS_OUTPUTS_DIR"
	harnessSecretsDirEnvVar = "SCION_HARNESS_SECRETS_DIR"
)

// k8sHomeFileMode selects how placeK8sHomeFiles delivers the staged files
// to their home targets.
type k8sHomeFileMode int

const (
	// k8sHomeFilesCopy copies each staged file to its target (plain pods).
	k8sHomeFilesCopy k8sHomeFileMode = iota
	// k8sHomeFilesLink only verifies that each target is the expected
	// symbolic link, or a user file the link step recorded as skipped
	// (NFS-home pods). It never writes to the home.
	k8sHomeFilesLink
)

// nfsHomePod reports whether config builds an NFS-home pod. Values other
// than "", "local" and HomeStorageNFS are rejected.
func nfsHomePod(config RunConfig) (bool, error) {
	switch config.HomeStorageBackend {
	case "", "local":
		return false, nil
	case HomeStorageNFS:
		if config.UnixUsername == "" {
			return false, fmt.Errorf("home storage %q requires a container user name", HomeStorageNFS)
		}
		return true, nil
	default:
		return false, fmt.Errorf("unsupported home storage backend %q", config.HomeStorageBackend)
	}
}

// k8sHomeFileModeFor returns the placement mode for config.
func k8sHomeFileModeFor(config RunConfig) k8sHomeFileMode {
	if ok, _ := nfsHomePod(config); ok {
		return k8sHomeFilesLink
	}
	return k8sHomeFilesCopy
}

// k8sHomeLink is one entry of SCION_HOME_LINKS: a link at Target (relative
// to the home) pointing to the staged file Source. Mode is the declared
// mode of the staged file, not of the link. It holds paths only.
type k8sHomeLink struct {
	Target string `json:"target"`
	Source string `json:"source"`
	Mode   string `json:"mode"`
}

// k8sHomeLinks returns the links an NFS-home pod requests, from the same
// placement list copy mode uses, so both modes cover the same targets.
func (r *KubernetesRuntime) k8sHomeLinks(config RunConfig) []k8sHomeLink {
	home := util.GetHomeDir(config.UnixUsername)
	placements := r.k8sHomeFilePlacements(config)
	out := make([]k8sHomeLink, 0, len(placements))
	for _, p := range placements {
		out = append(out, k8sHomeLink{
			Target: strings.TrimPrefix(p.Target, home+"/"),
			Source: p.Source,
			Mode:   fmt.Sprintf("%04o", p.Mode),
		})
	}
	return out
}

// k8sHomeLinkTargets returns the home-relative link targets for config, or
// nil for plain pods.
func (r *KubernetesRuntime) k8sHomeLinkTargets(config RunConfig) []string {
	if k8sHomeFileModeFor(config) != k8sHomeFilesLink {
		return nil
	}
	var out []string
	for _, l := range r.k8sHomeLinks(config) {
		out = append(out, l.Target)
	}
	return out
}

// k8sStagedPathFor returns the staged path of the projection with the given
// key that is placed in the home, or "" if there is none.
func (r *KubernetesRuntime) k8sStagedPathFor(config RunConfig, key string) string {
	for _, p := range r.k8sFileProjections(config) {
		if p.Key == key && placedInK8sHome(p.Target, config.UnixUsername) {
			return k8sStagingDir(p.Volume) + "/" + p.Key
		}
	}
	return ""
}

// nfsHomeVolume returns the memory-backed volume and its mount.
func nfsHomeVolume() (corev1.Volume, corev1.VolumeMount) {
	return corev1.Volume{
			Name: k8sMemVolume,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory},
			},
		}, corev1.VolumeMount{
			Name:      k8sMemVolume,
			MountPath: k8sMemDir,
		}
}

// nfsHomeEnv returns the env vars an NFS-home pod adds. They are appended
// after the plain values so they take precedence.
func (r *KubernetesRuntime) nfsHomeEnv(config RunConfig) ([]corev1.EnvVar, error) {
	links, err := json.Marshal(r.k8sHomeLinks(config))
	if err != nil {
		return nil, fmt.Errorf("failed to encode home links: %w", err)
	}
	env := []corev1.EnvVar{
		{Name: homeLinksEnvVar, Value: string(links)},
		{Name: harnessOutputsDirEnvVar, Value: k8sHarnessOutputsDir},
		{Name: harnessSecretsDirEnvVar, Value: k8sHarnessSecretsDir},
	}
	if p := r.k8sStagedPathFor(config, "secrets.json"); p != "" {
		env = append(env, corev1.EnvVar{Name: secretsFileEnvVar, Value: p})
	}
	return env, nil
}

// withHomeStorageAnnotation returns annotations with
// k8sHomeStorageAnnotation set when nfsHome is true and removed otherwise.
// The input map is not modified; it is returned as is when no change is
// needed.
func withHomeStorageAnnotation(annotations map[string]string, nfsHome bool) map[string]string {
	_, has := annotations[k8sHomeStorageAnnotation]
	if !nfsHome && !has {
		return annotations
	}
	out := make(map[string]string, len(annotations)+1)
	for k, v := range annotations {
		out[k] = v
	}
	delete(out, k8sHomeStorageAnnotation)
	if nfsHome {
		out[k8sHomeStorageAnnotation] = HomeStorageNFS
	}
	return out
}

// k8sHomeLinksResult is the content of k8sHomeLinksResultFile, written in
// the pod by the step that places the links. Targets are relative to the
// home. State is "linked" or "skipped" (a user file was kept at the
// target).
type k8sHomeLinksResult struct {
	Links []k8sHomeLinkResult `json:"links"`
}

type k8sHomeLinkResult struct {
	Target string `json:"target"`
	State  string `json:"state"`
}

const k8sHomeLinkSkipped = "skipped"

// k8sHomeLinkVerifyScript returns a POSIX shell script that checks each
// link target under home without following links. For a target that is not
// in skipped, every directory between home and the target must not be a
// symbolic link, and the target must be a symbolic link to its staged
// source. A target in skipped is one the link step left alone (a user's
// file, link or directory at the target, or a parent that is a symbolic
// link or not a directory) and is not checked. Anything else fails, naming
// the path. The script reads and writes nothing else.
func k8sHomeLinkVerifyScript(home string, links []k8sHomeLink, skipped map[string]bool) string {
	var b strings.Builder
	b.WriteString(`fail() {
	echo "home file check failed at $1: $2" >&2
	exit 1
}
parent() {
	if [ -L "$1" ]; then
		fail "$1" "parent directory is a symbolic link"
	fi
}
check() {
	t=$1; src=$2
	if [ -L "$t" ]; then
		l=$(readlink "$t") || fail "$t" "cannot read symbolic link"
		[ "$l" = "$src" ] || fail "$t" "symbolic link does not point to the staged file"
	elif [ -e "$t" ]; then
		fail "$t" "expected a symbolic link to the staged file"
	else
		fail "$t" "missing"
	fi
}
`)
	seen := map[string]bool{}
	for _, l := range links {
		if skipped[l.Target] {
			continue
		}
		for _, dir := range homeLinkParents(l.Target) {
			if !seen[dir] {
				seen[dir] = true
				fmt.Fprintf(&b, "parent %s\n", shellQuote(path.Join(home, dir)))
			}
		}
		fmt.Fprintf(&b, "check %s %s\n", shellQuote(path.Join(home, l.Target)), shellQuote(l.Source))
	}
	return b.String()
}

// homeLinkParents returns the directories between the home and the
// home-relative target, outermost first: "a/b/c" gives "a" and "a/b".
func homeLinkParents(target string) []string {
	var out []string
	dir := path.Dir(path.Clean(target))
	for dir != "." && dir != "/" {
		out = append([]string{dir}, out...)
		dir = path.Dir(dir)
	}
	return out
}

// k8sHomeLinksResultCommand prints k8sHomeLinksResultFile if it exists.
var k8sHomeLinksResultCommand = []string{"sh", "-c", "f=" + k8sHomeLinksResultFile + "; if [ -e \"$f\" ]; then cat \"$f\"; fi"}

// parseK8sHomeLinksResult returns the targets recorded as skipped. Empty
// output (no result file) records none.
func parseK8sHomeLinksResult(out string) (map[string]bool, error) {
	skipped := map[string]bool{}
	if strings.TrimSpace(out) == "" {
		return skipped, nil
	}
	var res k8sHomeLinksResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", k8sHomeLinksResultFile, err)
	}
	for _, l := range res.Links {
		if l.State == k8sHomeLinkSkipped {
			skipped[l.Target] = true
		}
	}
	return skipped, nil
}

// verifyK8sHomeLinks is link mode of placeK8sHomeFiles.
func (r *KubernetesRuntime) verifyK8sHomeLinks(ctx context.Context, namespace, podName string, config RunConfig) error {
	links := r.k8sHomeLinks(config)
	if len(links) == 0 {
		return nil
	}
	targets := make([]string, 0, len(links))
	for _, l := range links {
		targets = append(targets, l.Target)
	}
	runtimeLog.Info("Checking linked files in agent home", "agent", config.Name, "phase", "home-files", "targets", targets)
	out, err := r.execInPod(ctx, namespace, podName, k8sHomeLinksResultCommand)
	if err != nil {
		return fmt.Errorf("failed to read %s: %w", k8sHomeLinksResultFile, err)
	}
	skipped, err := parseK8sHomeLinksResult(out)
	if err != nil {
		return err
	}
	script := k8sHomeLinkVerifyScript(util.GetHomeDir(config.UnixUsername), links, skipped)
	if _, err := r.execInPod(ctx, namespace, podName, []string{"sh", "-c", script}); err != nil {
		return fmt.Errorf("failed to verify linked files in agent home: %w", err)
	}
	return nil
}

// homeFeatureToken is the sciontool capability NFS-home pods require: the
// "home leaf", "home prepare" and "home mark-seeded" contract. The init
// containers check the image's sciontool for it before using it.
const homeFeatureToken = "home-v1"

const (
	// k8sHomeVolume is the home's PVC volume when its claim differs from
	// the workspace claim.
	k8sHomeVolume = "scion-home"
	// k8sHomeLeafContainer creates the home directory (pod leaf mode).
	k8sHomeLeafContainer = "home-leaf"
	// k8sHomePrepareContainer prepares the home at every start.
	k8sHomePrepareContainer = "home-prepare"
	// k8sHomePrepareMount and k8sHomeAgentDirMount are where those init
	// containers mount the home and the agent directory.
	k8sHomePrepareMount  = "/scion-home"
	k8sHomeAgentDirMount = "/scion-agent-dir"
	// k8sHomeModeFile is written by home-prepare and read by the broker.
	k8sHomeModeFile = k8sMemDir + "/home-mode.json"
	// k8sHomeLeafPod and k8sHomeLeafBroker are HomeStorageRealization.Leaf
	// values.
	k8sHomeLeafPod    = "pod"
	k8sHomeLeafBroker = "broker"
)

// Env vars set on the home init containers only.
const (
	homeAgentIDEnvVar     = "SCION_HOME_AGENT_ID"
	homeStartIDEnvVar     = "SCION_HOME_START_ID"
	homeGIDEnvVar         = "SCION_HOME_GID"
	homeSkeletonSrcEnvVar = "SCION_HOME_SKELETON_SOURCE"
	homeSkeletonMaxEnvVar = "SCION_HOME_SKELETON_MAX_BYTES"
)

// validHomeAgentID reports whether id is a hub agent ID in canonical
// (lower-case, hyphenated) UUID form.
func validHomeAgentID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
	}
	return true
}

// NFSHomeSubPaths returns the agent directory
// <subPathRoot>/<projectID>/agents/<slug> and the agent's home directory
// in it, home-<agentID>, as paths relative to the export. Every component
// is validated: the subpath root must be a clean relative path, the
// project ID must pass shareddirs.ValidProjectID, the slug must be an
// agent slug (api.ValidateAgentName, as for NFSAgentDirSubPath), and the
// agent ID must be a canonical UUID that differs from the slug.
func NFSHomeSubPaths(subPathRoot, projectID, slug, agentID string) (agentDir, home string, err error) {
	if subPathRoot == "" || !filepath.IsLocal(subPathRoot) || filepath.Clean(subPathRoot) != subPathRoot {
		return "", "", fmt.Errorf("home storage nfs: invalid subpath root %q", subPathRoot)
	}
	if !shareddirs.ValidProjectID(projectID) {
		return "", "", fmt.Errorf("home storage nfs: invalid project ID %q", projectID)
	}
	if s, verr := api.ValidateAgentName(slug); verr != nil || s != slug {
		return "", "", fmt.Errorf("home storage nfs: agent name %q is not an agent slug", slug)
	}
	if !validHomeAgentID(agentID) || agentID == slug {
		return "", "", fmt.Errorf("home storage nfs: invalid agent ID %q", agentID)
	}
	agentDir = filepath.Join(subPathRoot, projectID, "agents", slug)
	return agentDir, filepath.Join(agentDir, "home-"+agentID), nil
}

// homeInitCommand wraps a sciontool home command in a check that the
// image's sciontool supports the contract, so an older image fails with a
// clear message instead of an unknown command.
func homeInitCommand(args ...string) []string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shellQuote(a)
	}
	script := fmt.Sprintf(`if sciontool version --features 2>/dev/null | grep -qx %s; then exec %s; fi
echo "home_prepare_failed: image sciontool lacks NFS-home support (needs %s)" >&2
exit 3`, shellQuote(homeFeatureToken), strings.Join(quoted, " "), homeFeatureToken)
	return []string{"sh", "-c", script}
}

// nfsHomePodParts is what buildPod adds to an NFS-home pod.
type nfsHomePodParts struct {
	// volume is set when the home needs its own PVC volume.
	volume *corev1.Volume
	// mount is the agent container's home mount.
	mount corev1.VolumeMount
	// initContainers run first, in order.
	initContainers []corev1.Container
}

// nfsHomePodSpec builds the home volume, the agent container's home mount
// and the home init containers. workspaceClaim is the claim of the
// workspace volume "workspace", or "" when the workspace is not a PVC; the
// home shares that volume when the claims match.
func (r *KubernetesRuntime) nfsHomePodSpec(config RunConfig, containerHome, workspaceClaim string) (nfsHomePodParts, error) {
	var parts nfsHomePodParts
	hs := config.HomeStorage
	if hs == nil || hs.PVClaimName == "" {
		return parts, fmt.Errorf("home storage %q needs a claim", HomeStorageNFS)
	}
	agentDir, home, err := NFSHomeSubPaths(hs.SubPathRoot, hs.ProjectID, hs.AgentSlug, hs.AgentID)
	if err != nil {
		return parts, err
	}
	if hs.GID <= 0 {
		return parts, fmt.Errorf("home storage %q needs the export group", HomeStorageNFS)
	}
	startID := config.Labels[labelStartID]
	if startID == "" {
		return parts, fmt.Errorf("home storage %q needs a start ID", HomeStorageNFS)
	}

	volName := k8sHomeVolume
	if workspaceClaim != "" && workspaceClaim == hs.PVClaimName {
		volName = "workspace"
	} else {
		parts.volume = &corev1.Volume{
			Name: volName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: hs.PVClaimName},
			},
		}
	}
	parts.mount = corev1.VolumeMount{Name: volName, MountPath: containerHome, SubPath: home}

	noEscalation := false
	if hs.Leaf == k8sHomeLeafPod {
		parts.initContainers = append(parts.initContainers, corev1.Container{
			Name:    k8sHomeLeafContainer,
			Image:   config.Image,
			Command: homeInitCommand("sciontool", "home", "leaf", "--agent-dir", k8sHomeAgentDirMount),
			Env: []corev1.EnvVar{
				{Name: homeAgentIDEnvVar, Value: hs.AgentID},
				{Name: homeGIDEnvVar, Value: strconv.Itoa(hs.GID)},
			},
			VolumeMounts:             []corev1.VolumeMount{{Name: volName, MountPath: k8sHomeAgentDirMount, SubPath: agentDir}},
			TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
			// The same security context as the workspace provisioning init
			// container: root with only the ownership capabilities.
			SecurityContext: &corev1.SecurityContext{
				RunAsUser:                int64Ptr(0),
				RunAsGroup:               int64Ptr(0),
				RunAsNonRoot:             boolPtr(false),
				AllowPrivilegeEscalation: &noEscalation,
				Capabilities: &corev1.Capabilities{
					Drop: []corev1.Capability{"ALL"},
					Add:  []corev1.Capability{"CHOWN", "FOWNER", "DAC_OVERRIDE"},
				},
			},
		})
	} else if hs.Leaf != k8sHomeLeafBroker {
		return parts, fmt.Errorf("home storage %q: unknown leaf mode %q", HomeStorageNFS, hs.Leaf)
	}

	links, err := json.Marshal(r.k8sHomeLinks(config))
	if err != nil {
		return parts, fmt.Errorf("failed to encode home links: %w", err)
	}
	skeletonMax := hs.SkeletonMaxBytes
	_, memMount := nfsHomeVolume()
	parts.initContainers = append(parts.initContainers, corev1.Container{
		Name:    k8sHomePrepareContainer,
		Image:   config.Image,
		Command: homeInitCommand("sciontool", "home", "prepare", "--home", k8sHomePrepareMount, "--mem-dir", k8sMemDir),
		Env: []corev1.EnvVar{
			{Name: homeAgentIDEnvVar, Value: hs.AgentID},
			{Name: homeStartIDEnvVar, Value: startID},
			{Name: homeLinksEnvVar, Value: string(links)},
			{Name: homeSkeletonSrcEnvVar, Value: containerHome},
			{Name: homeSkeletonMaxEnvVar, Value: strconv.FormatInt(skeletonMax, 10)},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: volName, MountPath: k8sHomePrepareMount, SubPath: home},
			memMount,
		},
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                int64Ptr(containerUID),
			RunAsGroup:               int64Ptr(containerUID),
			RunAsNonRoot:             boolPtr(true),
			AllowPrivilegeEscalation: &noEscalation,
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	})
	return parts, nil
}

// checkHomeMounts allows exactly one mount at the home path, the home
// mount itself on the agent container, and rejects any other mount at or
// under the home in any container of the pod. On an NFS-home pod the home
// is the export, so another mount there would hide it or be created
// inside it.
func checkHomeMounts(pod *corev1.Pod, home string, homeMount corev1.VolumeMount) error {
	check := func(container string, mounts []corev1.VolumeMount, isAgent bool) error {
		for _, vm := range mounts {
			p := path.Clean(vm.MountPath)
			if p != home && !strings.HasPrefix(p, home+"/") {
				continue
			}
			if isAgent && vm == homeMount {
				continue
			}
			return fmt.Errorf("volume %q is mounted at %s in container %q, at or inside the agent home %s; with home storage %q only the home volume may be mounted there",
				vm.Name, vm.MountPath, container, home, HomeStorageNFS)
		}
		return nil
	}
	found := 0
	for i, c := range pod.Spec.Containers {
		if err := check(c.Name, c.VolumeMounts, i == 0); err != nil {
			return err
		}
		if i == 0 {
			for _, vm := range c.VolumeMounts {
				if vm == homeMount {
					found++
				}
			}
		}
	}
	for _, c := range pod.Spec.InitContainers {
		if err := check(c.Name, c.VolumeMounts, false); err != nil {
			return err
		}
	}
	if found != 1 {
		return fmt.Errorf("the agent home %s must be mounted exactly once in the agent container (found %d)", home, found)
	}
	return nil
}

// homeKeptOnExport reports whether a pod's annotations mark an NFS-home
// pod, whose home Sync does not copy.
func homeKeptOnExport(annotations map[string]string) bool {
	return annotations[k8sHomeStorageAnnotation] == HomeStorageNFS
}

// isNFSHomePod reports whether pod carries the NFS-home annotation.
func isNFSHomePod(pod *corev1.Pod) bool {
	return pod != nil && pod.Annotations[k8sHomeStorageAnnotation] == HomeStorageNFS
}

// podDeleteOptions returns the delete options for pod. An NFS-home pod is
// never force-deleted: it is deleted with its own grace period, so its
// containers stop writing to the home before another pod of the agent can
// start. Other pods keep the immediate deletion.
func podDeleteOptions(pod *corev1.Pod) metav1.DeleteOptions {
	if isNFSHomePod(pod) {
		return metav1.DeleteOptions{}
	}
	zero := int64(0)
	return metav1.DeleteOptions{GracePeriodSeconds: &zero}
}

// k8sHomeModeCommand prints the mode file home-prepare wrote.
var k8sHomeModeCommand = []string{"cat", k8sHomeModeFile}

// k8sHomeMode is the content of k8sHomeModeFile.
type k8sHomeMode struct {
	Mode    string `json:"mode"`
	StartID string `json:"start_id"`
}

// Home modes home-prepare chooses.
const (
	k8sHomeModeSeed     = "seed"
	k8sHomeModeSeedOver = "seed-over"
)

// parseK8sHomeMode parses the mode file and checks that it belongs to this
// start and names a known mode.
func parseK8sHomeMode(out, startID string) (string, error) {
	var m k8sHomeMode
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		return "", fmt.Errorf("invalid %s: %w", k8sHomeModeFile, err)
	}
	if m.StartID != startID {
		return "", fmt.Errorf("%s belongs to start %q, not this start", k8sHomeModeFile, m.StartID)
	}
	switch m.Mode {
	case k8sHomeModeSeed, k8sHomeModeSeedOver:
		return m.Mode, nil
	}
	return "", fmt.Errorf("%s names an unknown mode %q", k8sHomeModeFile, m.Mode)
}

// k8sHomeMarkSeededCommand marks the home seeded after the first transfer.
func k8sHomeMarkSeededCommand(home, agentID, startID string) []string {
	return []string{"sciontool", "home", "mark-seeded", "--home", home, "--agent-id", agentID, "--start-id", startID}
}

// Termination wait.
//
// Before a start creates the agent's pod, the previous pod of the agent
// must have stopped, so two pods never write to the same home. The
// previous NFS-home pod is deleted with its grace period; the start then
// waits until termination is confirmed, for at most the grace period plus
// the configured wait. A pod whose node is lost is refused at once.

// errPreviousPodUnconfirmed is the retryable refusal when the previous pod
// of an NFS-home agent has not been confirmed stopped.
var errPreviousPodUnconfirmed = errors.New("previous_pod_unconfirmed")

// k8sTerminationPollInterval is how often the wait reads the previous pod.
var k8sTerminationPollInterval = time.Second

// podTerminationConfirmed reports whether pod's containers are confirmed
// stopped: the pod was never bound to a node, or every container named in
// its spec (init, main and ephemeral) has a status entry and that entry is
// terminated. Empty or partial status lists are not confirmation.
func podTerminationConfirmed(pod *corev1.Pod) bool {
	if pod.Spec.NodeName == "" {
		return true
	}
	states := map[string]bool{}
	for _, list := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses, pod.Status.EphemeralContainerStatuses} {
		for _, cs := range list {
			states[cs.Name] = cs.State.Terminated != nil
		}
	}
	var names []string
	for _, c := range pod.Spec.InitContainers {
		names = append(names, c.Name)
	}
	for _, c := range pod.Spec.Containers {
		names = append(names, c.Name)
	}
	for _, c := range pod.Spec.EphemeralContainers {
		names = append(names, c.Name)
	}
	for _, n := range names {
		if terminated, ok := states[n]; !ok || !terminated {
			return false
		}
	}
	return true
}

// k8sNodeNotReadyReason is the Ready condition reason the node lifecycle
// controller sets on pods of a node that stopped reporting.
const k8sNodeNotReadyReason = "NodeNotReady"

// podNodeLost reports whether pod shows that its node is lost, from the
// pod's own conditions (no node read access is needed): a true
// DisruptionTarget condition, or Ready false with the node lifecycle
// reason.
func podNodeLost(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.DisruptionTarget && c.Status == corev1.ConditionTrue {
			return true
		}
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionFalse && c.Reason == k8sNodeNotReadyReason {
			return true
		}
	}
	return false
}

// Defaults for HomeStorageRealization values left at zero.
const (
	defaultHomeStopGraceSeconds       = 30
	defaultHomeTerminationWaitSeconds = 15
)

// homeTerminationGrace returns the grace period of NFS-home pods.
func homeTerminationGrace(hs *HomeStorageRealization) int {
	if hs != nil && hs.StopGraceSeconds > 0 {
		return hs.StopGraceSeconds
	}
	return defaultHomeStopGraceSeconds
}

// waitForPodTermination waits until the pod with uid is gone or confirmed
// stopped, for at most bound. It refuses with errPreviousPodUnconfirmed at
// once when the pod shows a lost node, and when the bound runs out.
func (r *KubernetesRuntime) waitForPodTermination(ctx context.Context, namespace, podName string, uid types.UID, bound time.Duration) error {
	clock := r.execReadyClock
	if clock.now == nil || clock.sleep == nil {
		clock = realExecReadyClock()
	}
	deadline := clock.now().Add(bound)
	for {
		pod, err := r.Client.Clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
		switch {
		case k8serrors.IsNotFound(err):
			return nil
		case err != nil:
			if !clock.now().Before(deadline) {
				return fmt.Errorf("%w: cannot read the previous pod %s: %v", errPreviousPodUnconfirmed, podName, err)
			}
			runtimeLog.Debug("Reading the previous pod failed; retrying", "pod", podName, "error", err)
		case pod.UID != uid:
			// The name now belongs to another pod: the one we waited for is gone.
			return nil
		case podTerminationConfirmed(pod):
			return nil
		case podNodeLost(pod):
			return fmt.Errorf("%w: the previous pod %s is on a node that is not reachable; retry once the node is back or the pod is gone", errPreviousPodUnconfirmed, podName)
		}
		if !clock.now().Before(deadline) {
			return fmt.Errorf("%w: the previous pod %s did not stop within %s; retry later", errPreviousPodUnconfirmed, podName, bound)
		}
		if err := clock.sleep(ctx, k8sTerminationPollInterval); err != nil {
			return err
		}
	}
}

// errAgentStartInProgress is the retryable refusal when another start of
// the same NFS-home agent holds the start lock.
var errAgentStartInProgress = errors.New("agent_start_in_progress")

// acquireHomeStartLock takes the per-agent start lock of an NFS-home agent
// and returns its release. Without a locker (no shared store) it is a
// no-op: the termination wait and the unique pod name are then the guards.
func acquireHomeStartLock(ctx context.Context, config RunConfig) (func(), error) {
	if config.Locker == nil || config.HomeStorage == nil {
		runtimeLog.Debug("No advisory locker; NFS-home start is guarded by the termination wait only", "agent", config.Name)
		return func() {}, nil
	}
	objID := store.StableProjectHash(config.HomeStorage.ProjectID + "/" + config.HomeStorage.AgentID)
	acquired, release, err := config.Locker.TryAdvisoryLockObject(ctx, store.LockAgentHomeStart, objID)
	if err != nil {
		return nil, fmt.Errorf("agent start lock for %s: %w", config.Name, err)
	}
	if !acquired {
		return nil, fmt.Errorf("%w: another start of agent %s is in progress; retry", errAgentStartInProgress, config.Name)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			if err := release(); err != nil {
				runtimeLog.Error("Failed to release the agent start lock", "agent", config.Name, "error", err)
			}
		})
	}, nil
}

// k8sHomeHooksGuardCommand fails when .scion or .scion/hooks in the home is
// a symbolic link, so the seed-over transfer never writes hook scripts
// through a link. It runs before the transfer, while no agent process runs
// in the pod.
func k8sHomeHooksGuardCommand(home string) []string {
	return []string{"sh", "-c", `for p in "$1/.scion" "$1/.scion/hooks"; do
	if [ -L "$p" ]; then
		echo "home_prepare_failed: $p is a symbolic link in the agent home; the home is not transferred through it" >&2
		exit 1
	fi
done`, "sh", home}
}
