package runtime

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// Kubernetes file staging and placement.
//
// Agent pods run as the image's non-root user. When a file (a file-type
// secret, a harness auth file, or secrets.json) is mounted with subPath at a
// path inside the agent home and the parent directory is not in the image,
// the container runtime creates that directory owned by root. The home sync
// (tar over exec, running as the pod user) then cannot create entries in it,
// and the post-sync chown cannot repair it either.
//
// To keep the home writable by the pod user, the volumes that carry these
// files are mounted under k8sFileStagingRoot instead, and the files are
// copied to their home targets by placeK8sHomeFiles after the home sync, as
// the pod user. Targets outside the home keep their direct subPath mounts.
const k8sFileStagingRoot = "/run/scion"

// Volume names for the file-carrying volumes, shared by buildPod and the
// placement list so both are derived from the same data.
const (
	k8sSecretsStoreVolume = "secrets-store"
	k8sAgentSecretsVolume = "agent-secrets"
	k8sAuthFilesVolume    = "auth-files"
)

// k8sPlacedFileMode is the mode of every file placed in the agent home. All
// placed files are credentials or secrets.
const k8sPlacedFileMode = 0o600

// k8sStagingDir returns the directory a file-carrying volume is mounted at.
func k8sStagingDir(volume string) string {
	return k8sFileStagingRoot + "/" + volume
}

// k8sFileProjection is one file that a pod volume delivers to a path in the
// agent container.
type k8sFileProjection struct {
	Volume string // pod volume name (one of the k8s*Volume constants)
	Key    string // file name within the volume (Secret key or CSI object)
	Target string // absolute container path
}

// k8sFilePlacement is one copy, performed in the pod after the home sync,
// from a staged volume file to its target in the agent home.
type k8sFilePlacement struct {
	Source string
	Target string
	Mode   uint32
}

// useGKESecretsPath reports whether secrets are delivered through the
// Secrets Store CSI driver (GKE mode with at least one external reference)
// rather than a plain Kubernetes Secret.
func (r *KubernetesRuntime) useGKESecretsPath(config RunConfig) bool {
	if !r.GKEMode {
		return false
	}
	for _, s := range config.ResolvedSecrets {
		if s.Ref != "" {
			return true
		}
	}
	return false
}

// k8sFileProjections lists every file that the secret and auth-file volumes
// deliver to the agent container, in a stable order.
func (r *KubernetesRuntime) k8sFileProjections(config RunConfig) []k8sFileProjection {
	containerHome := util.GetHomeDir(config.UnixUsername)
	var out []k8sFileProjection

	if len(config.ResolvedSecrets) > 0 {
		gke := r.useGKESecretsPath(config)
		hasVariable := false
		for _, s := range config.ResolvedSecrets {
			switch s.Type {
			case "file":
				vol := k8sAgentSecretsVolume
				if gke {
					vol = k8sSecretsStoreVolume
				}
				out = append(out, k8sFileProjection{
					Volume: vol,
					Key:    s.Name,
					Target: expandTildeTarget(s.Target, containerHome),
				})
			case "variable":
				hasVariable = true
			}
		}
		// The CSI path does not deliver variable secrets (unchanged behaviour).
		if hasVariable && !gke {
			out = append(out, k8sFileProjection{
				Volume: k8sAgentSecretsVolume,
				Key:    "secrets.json",
				Target: path.Join(containerHome, ".scion", "secrets.json"),
			})
		}
	}

	if config.ResolvedAuth != nil {
		for i, f := range config.ResolvedAuth.Files {
			if f.SourcePath == "" {
				continue
			}
			out = append(out, k8sFileProjection{
				Volume: k8sAuthFilesVolume,
				Key:    fmt.Sprintf("auth-file-%d", i),
				Target: expandTildeTarget(f.ContainerPath, containerHome),
			})
		}
	}
	return out
}

// placedInK8sHome reports whether a projection target is delivered by copy
// into the agent home rather than by a direct subPath mount. An empty
// UnixUsername has no per-user home (util.GetHomeDir("") is "/home"), so
// those targets keep their direct mounts.
func placedInK8sHome(target, unixUsername string) bool {
	if unixUsername == "" {
		return false
	}
	home := util.GetHomeDir(unixUsername)
	clean := path.Clean(target)
	return strings.HasPrefix(clean, home+"/")
}

// k8sHomeFilePlacements returns the copies placeK8sHomeFiles performs, built
// from the same projection list buildPod uses for the volume mounts.
func (r *KubernetesRuntime) k8sHomeFilePlacements(config RunConfig) []k8sFilePlacement {
	var out []k8sFilePlacement
	for _, p := range r.k8sFileProjections(config) {
		if !placedInK8sHome(p.Target, config.UnixUsername) {
			continue
		}
		out = append(out, k8sFilePlacement{
			Source: k8sStagingDir(p.Volume) + "/" + p.Key,
			Target: path.Clean(p.Target),
			Mode:   k8sPlacedFileMode,
		})
	}
	return out
}

// checkK8sHomeFileTargets rejects two placements with the same home target.
// With direct mounts these were two volume mounts at one path, which the API
// server refuses; with placement the later copy would silently win.
func checkK8sHomeFileTargets(placements []k8sFilePlacement) error {
	seen := make(map[string]bool, len(placements))
	for _, p := range placements {
		if seen[p.Target] {
			return fmt.Errorf("more than one secret or auth file targets %s", p.Target)
		}
		seen[p.Target] = true
	}
	return nil
}

// k8sFilePlacementScript returns a POSIX shell script that copies each
// placement's source to its target under home. Placement requires real
// directories and does not follow symlinks: every directory between home and
// the directory that holds the file is checked in turn, and a symlink or a
// non-directory there, or a symlink at the target itself, stops placement.
// A missing directory is created by the running user: the directory that
// directly holds the file gets 0700, any missing ancestors get the default
// mode (0777 masked by the umask). Existing directories are left as they
// are. The holding directory must be writable. An existing regular file at
// the target is replaced; anything else at the target stops placement. The
// script stops at the first failure and writes the target path and the
// reason (never file contents) to stderr.
func k8sFilePlacementScript(home string, placements []k8sFilePlacement) string {
	var b strings.Builder
	b.WriteString(`fail() {
	echo "failed to place file at $dst: $1" >&2
	return 1
}
place() {
	home=$1; src=$2; dst=$3; mode=$4; dir=${dst%/*}
	cur=$home
	rest=
	[ "$dir" = "$home" ] || rest=${dir#"$home"/}
	while [ -n "$rest" ]; do
		case $rest in
		*/*) c=${rest%%/*}; rest=${rest#*/} ;;
		*) c=$rest; rest= ;;
		esac
		cur=$cur/$c
		if [ -L "$cur" ]; then
			fail "$cur is a symbolic link; placement requires real directories"
			return 1
		elif [ -e "$cur" ]; then
			[ -d "$cur" ] || { fail "$cur is not a directory"; return 1; }
		elif [ "$cur" = "$dir" ]; then
			mkdir -m 0700 "$cur" || { fail "cannot create directory $cur"; return 1; }
		else
			mkdir "$cur" || { fail "cannot create directory $cur"; return 1; }
		fi
	done
	[ -w "$dir" ] || { fail "directory $dir is not writable"; return 1; }
	if [ -L "$dst" ]; then
		fail "target is a symbolic link; placement does not follow symlinks"
		return 1
	fi
	if [ -e "$dst" ] && [ ! -f "$dst" ]; then
		fail "target exists and is not a regular file"
		return 1
	fi
	rm -f "$dst" || { fail "cannot remove existing file"; return 1; }
	(umask 077 && cp "$src" "$dst") || { fail "cannot copy file"; return 1; }
	chmod "$mode" "$dst" || { fail "cannot set mode"; return 1; }
}
`)
	for _, p := range placements {
		fmt.Fprintf(&b, "place %s %s %s %04o || exit 1\n",
			shellQuote(home), shellQuote(p.Source), shellQuote(p.Target), p.Mode)
	}
	return b.String()
}

// placeK8sHomeFiles delivers the staged secret and auth files to their
// targets in the agent home, as the pod user. It runs after the home sync
// and before the startup gate is signalled, so the harness sees the files at
// the same paths a direct mount would have used. In copy mode it copies each
// file; in link mode (NFS-home pods, see k8s_nfs_home.go) it only verifies
// the links. Only target paths are logged.
func (r *KubernetesRuntime) placeK8sHomeFiles(ctx context.Context, namespace, podName string, config RunConfig, mode k8sHomeFileMode) error {
	if mode == k8sHomeFilesLink {
		return r.verifyK8sHomeLinks(ctx, namespace, podName, config)
	}
	placements := r.k8sHomeFilePlacements(config)
	if len(placements) == 0 {
		return nil
	}
	targets := make([]string, 0, len(placements))
	for _, p := range placements {
		targets = append(targets, p.Target)
	}
	runtimeLog.Info("Placing files in agent home", "agent", config.Name, "phase", "home-files", "targets", targets)
	script := k8sFilePlacementScript(util.GetHomeDir(config.UnixUsername), placements)
	if _, err := r.execInPod(ctx, namespace, podName, []string{"sh", "-c", script}); err != nil {
		return fmt.Errorf("failed to place files in agent home: %w", err)
	}
	return nil
}
