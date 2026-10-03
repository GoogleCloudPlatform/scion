package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	corev1 "k8s.io/api/core/v1"

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

// checkNoMountsUnderHome rejects any agent container volume mount under the
// home. On an NFS-home pod the home is the export, and a mount under it
// would be created inside the export.
func checkNoMountsUnderHome(pod *corev1.Pod, home string) error {
	for _, vm := range pod.Spec.Containers[0].VolumeMounts {
		if strings.HasPrefix(path.Clean(vm.MountPath), home+"/") {
			return fmt.Errorf("volume %q is mounted at %s, inside the agent home %s; mounts inside the home are not supported with home storage %q",
				vm.Name, vm.MountPath, home, HomeStorageNFS)
		}
	}
	return nil
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
// link target under home without following links: a symbolic link must
// point to its staged source; a target in skipped must be a regular file;
// anything else fails, naming the target. The script reads and writes
// nothing else.
func k8sHomeLinkVerifyScript(home string, links []k8sHomeLink, skipped map[string]bool) string {
	var b strings.Builder
	b.WriteString(`fail() {
	echo "home file check failed at $1: $2" >&2
	exit 1
}
check() {
	t=$1; src=$2; skip=$3
	if [ -L "$t" ]; then
		l=$(readlink "$t") || fail "$t" "cannot read symbolic link"
		[ "$l" = "$src" ] || fail "$t" "symbolic link does not point to the staged file"
	elif [ "$skip" = 1 ] && [ -f "$t" ]; then
		:
	elif [ -e "$t" ]; then
		fail "$t" "expected a symbolic link to the staged file"
	else
		fail "$t" "missing"
	fi
}
`)
	for _, l := range links {
		skip := "0"
		if skipped[l.Target] {
			skip = "1"
		}
		fmt.Fprintf(&b, "check %s %s %s\n", shellQuote(path.Join(home, l.Target)), shellQuote(l.Source), skip)
	}
	return b.String()
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
