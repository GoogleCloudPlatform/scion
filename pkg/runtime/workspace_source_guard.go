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
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/util/fsutil"
)

// ValidateWorkspaceSource enforces the rule that a workspace source host path
// must resolve to a path under the caller's own project. It is the single
// shared, fail-closed gate for every runtime and every code path that turns a
// workspace source into a bind mount, sync operation, or upload — call it
// before any of those happen, not after, and act on the resolved path it
// returns rather than the original argument.
//
// On success it returns the resolved, symlink-free form of source (see
// resolveForValidation): the caller should use that value for the actual
// mount, sync, or upload, not the original source. This narrows the window
// between "the path we checked" and "the path we acted on" to components
// that change after this call returns — an ancestor directory replaced with
// a symlink, or the not-yet-created suffix of a path that did not exist at
// validation time, are not covered by this narrowing (see resolveForValidation
// and the callers that document this residual gap for their own call site).
// On failure it returns ("", err).
//
// roots are the per-project root(s) the source may fall under, when the
// caller has any. A caller with more than one legitimate root for a single
// source — for example a git repo root and a separately-verified worktree
// location — passes all of them. The floors below decide first, in the
// order the code actually runs them, regardless of whether any roots were
// passed:
//   - A source resolving to the top of its filesystem (the Unix '/', or a
//     Windows volume root such as `C:\`) refuses the whole call outright.
//   - A source naming one of fsutil's critical system paths (see
//     fsutil.IsCriticalSystemPath) refuses the whole call outright.
//   - A source equal to, or a strict ancestor of, $HOME refuses the whole
//     call outright.
//   - A source equal to, or a strict ancestor of, ~/.scion refuses the
//     whole call outright.
//   - A source under ~/.scion (bare or any descendant, and not already
//     refused by the ancestor floor just above) must be in the named
//     allow list (see isScionHomeAllowedSubtree) regardless of roots — a
//     hub-managed project workspace under ~/.scion/projects/<slug> or the
//     global project's own ~/.scion/workspace, for example, is on that
//     list and passes; anything else under ~/.scion is refused here,
//     before roots are even considered.
//   - A root resolving to the top of its filesystem, to $HOME, or to
//     ~/.scion refuses the whole call outright — a caller should never be
//     supplying one of those as "a root", and treating it as equivalent to
//     not supplying one would silently admit anything.
//
// Once those floors are satisfied, roots decide the rest:
//   - With one or more usable roots, the source must also fall under at
//     least one of them — containment. A source already accepted by the
//     ~/.scion allow list still needs this when a root is supplied (e.g.
//     the global project's own workspace, checked against its own root);
//     the allow list and containment agreeing is expected, not redundant.
//   - With no usable roots at all (or only empty strings), the floors
//     above are the only checks — a caller in this position cannot derive
//     a per-project root at this call site, so anything outside ~/.scion
//     entirely is accepted, and anything under ~/.scion falls to the
//     allow list alone.
//
// Both source and every root are resolved through any symlinks before any
// check runs, so a symlink that merely sits inside an allowed root but
// points outside it (or vice versa) is judged by where it actually leads,
// not by its nominal location. Resolution failure — including an
// unresolvable home directory, needed for the $HOME/~/.scion checks — is
// treated the same as a denied path: fail closed, not fail open.
//
// source may be empty (nothing to mount/sync/upload); that is not an error
// here — it returns ("", nil) — callers only invoke this once they have a
// non-empty source to act on.
func ValidateWorkspaceSource(source string, roots ...string) (string, error) {
	return validateHostPathSource(source, "workspace", isScionHomeAllowedSubtree, roots...)
}

// ValidateAgentHomeSource enforces the exact same floors and root rules as
// ValidateWorkspaceSource (see its doc comment), with one difference: within
// ~/.scion, it allows a single agent's own home subtree (see
// isScionHomeAllowedHomeSubtree) rather than ValidateWorkspaceSource's
// workspace-bearing subtrees. A workspace and an agent's home are never the
// same directory and never interchangeable under ~/.scion — none of
// ValidateWorkspaceSource's allowed shapes name a ".../home" leaf, and no
// real agent home is named "workspace", "projects/<slug>/workspace", or any
// of the other shapes that allow list admits — so reusing it here would both
// wrongly refuse every real agent home under ~/.scion and, the other
// direction, risk wrongly admitting a workspace path as if it were a home.
// Call this for any host path about to become an agent's home directory
// (a sync source or destination, a bind mount, or similar); call
// ValidateWorkspaceSource for a workspace path instead.
func ValidateAgentHomeSource(source string, roots ...string) (string, error) {
	return validateHostPathSource(source, "agent home", isScionHomeAllowedHomeSubtree, roots...)
}

// validateHostPathSource is the shared implementation behind
// ValidateWorkspaceSource and ValidateAgentHomeSource. kind names the kind
// of path being validated for error messages ("workspace" or "agent home"),
// and allowedScionHomeSubtree decides which ~/.scion subtree the two allow,
// since a workspace and an agent home are never the same shape (see
// ValidateAgentHomeSource's doc comment).
func validateHostPathSource(source, kind string, allowedScionHomeSubtree func(resolvedSource, scionHomeDir string) bool, roots ...string) (string, error) {
	if source == "" {
		return "", nil
	}

	// A relative source resolves relative to the current directory (both
	// filepath.Clean and filepath.EvalSymlinks preserve that), so it is
	// never equal to '/', $HOME, or a ~/.scion prefix, and the whole floor
	// below is skipped by construction. Every legitimate producer of a
	// source already passes an absolute path; refuse anything else outright
	// rather than resolve it against an ambient, caller dependent working
	// directory.
	if !filepath.IsAbs(source) {
		return "", fmt.Errorf("%s source %q is not an allowed %s path", kind, source, kind)
	}

	resolvedSource, err := resolveForValidation(source)
	if err != nil {
		return "", fmt.Errorf("%s source %q is not an allowed %s path", kind, source, kind)
	}

	if isFilesystemRoot(resolvedSource) {
		return "", fmt.Errorf("%s source %q is not an allowed %s path", kind, source, kind)
	}

	// A source naming one of fsutil's criticalSystemPaths is refused outright,
	// the same way '/' is above: accepting any of these as a source would
	// mount or sync a whole system directory instead of one project's own
	// files. This reuses fsutil's list rather than keeping a second copy of
	// it that could drift out of sync.
	if fsutil.IsCriticalSystemPath(resolvedSource) {
		return "", fmt.Errorf("%s source %q is not an allowed %s path", kind, source, kind)
	}

	home, homeErr := os.UserHomeDir()
	if homeErr != nil || home == "" {
		return "", fmt.Errorf("%s source %q is not an allowed %s path: home directory could not be determined", kind, source, kind)
	}
	cleanHome, err := resolveForValidation(home)
	if err != nil {
		return "", fmt.Errorf("%s source %q is not an allowed %s path: home directory could not be resolved", kind, source, kind)
	}

	if pathEqualFailSafe(resolvedSource, cleanHome) {
		return "", fmt.Errorf("%s source %q is not an allowed %s path", kind, source, kind)
	}

	// A source that is a strict ANCESTOR of $HOME -- not just equal to it --
	// would still admit the user's entire home directory if accepted (every
	// file under $HOME lives under such a source too): refuse that the same
	// way, exact spelling and case-fold. pathIsOrUnderCaseFold's arguments
	// are swapped from their usual (source, ancestor) order here, since the
	// question is whether cleanHome sits under resolvedSource, the reverse
	// of every other call to it in this file.
	if strings.HasPrefix(cleanHome, resolvedSource+string(filepath.Separator)) || pathIsOrUnderCaseFold(cleanHome, resolvedSource) {
		return "", fmt.Errorf("%s source %q is not an allowed %s path", kind, source, kind)
	}

	// ~/.scion (bare or any descendant) is a universal floor, the same as
	// '/' and $HOME above, not something a caller-supplied root can satisfy
	// by containment: a source under ~/.scion must be in the narrow, named
	// allow list (allowedScionHomeSubtree) regardless of whether a root was
	// supplied at all, and regardless of whether that root happens to also
	// resolve into ~/.scion (a project whose own repo is, unusually, a git
	// work tree rooted at ~/.scion -- a real dotfiles pattern -- or a
	// registered worktree of that repo that itself resolves to ~/.scion). A
	// source outside ~/.scion entirely is unaffected by this check either
	// way.
	//
	// Resolution failure here fails closed, the same as cleanHome above: a
	// silent fallback to the unresolved, lexical ~/.scion join would let a
	// resolvedSource that legitimately resolved through a real symlinked
	// ~/.scion miss the HasPrefix comparison against the lexical form,
	// skipping the floor entirely. resolveForValidation itself already
	// walks up to the nearest existing ancestor when ~/.scion simply
	// doesn't exist yet, so a genuine error here means something is
	// actually wrong (a broken symlink, a permission error), not that the
	// directory is merely uncreated -- exactly the case this floor must not
	// silently wave through.
	scionHomeDir, err := resolveForValidation(filepath.Join(cleanHome, ".scion"))
	if err != nil {
		return "", fmt.Errorf("%s source %q is not an allowed %s path: ~/.scion could not be resolved", kind, source, kind)
	}

	// A source that is a strict ancestor of ~/.scion is refused the same way
	// as an ancestor of $HOME above, and for the same reason: every path
	// under ~/.scion -- including everything the allow list below protects
	// -- would also sit under such a source.
	if strings.HasPrefix(scionHomeDir, resolvedSource+string(filepath.Separator)) || pathIsOrUnderCaseFold(scionHomeDir, resolvedSource) {
		return "", fmt.Errorf("%s source %q is not an allowed %s path", kind, source, kind)
	}

	if resolvedSource == scionHomeDir || strings.HasPrefix(resolvedSource, scionHomeDir+string(filepath.Separator)) {
		// Exact, case-preserving match: component names can be trusted, so
		// the named allow list decides.
		if !allowedScionHomeSubtree(resolvedSource, scionHomeDir) {
			return "", fmt.Errorf("%s source %q is not an allowed %s path", kind, source, kind)
		}
	} else if pathIsOrUnderCaseFold(resolvedSource, scionHomeDir) {
		// Same file per a case-insensitive filesystem (macOS, Windows), but
		// not a byte-for-byte match: filepath.Rel-based component matching
		// in allowedScionHomeSubtree cannot be trusted to name the right
		// components here, so this fails closed outright rather than
		// attempting the allow list against a spelling it wasn't computed
		// for. A source outside ~/.scion by any spelling is unaffected.
		return "", fmt.Errorf("%s source %q is not an allowed %s path", kind, source, kind)
	}

	var usableRoots []string
	for _, root := range roots {
		if root == "" {
			continue
		}
		if !filepath.IsAbs(root) {
			// Same reasoning as the source check above: a relative root
			// can never be meaningfully compared for containment, and a
			// caller should never be supplying one. Refuse the whole call.
			return "", fmt.Errorf("%s source %q is outside the permitted %s root", kind, source, kind)
		}
		resolvedRoot, err := resolveForValidation(root)
		if err != nil {
			return "", fmt.Errorf("%s source %q is outside the permitted %s root", kind, source, kind)
		}
		if isFilesystemRoot(resolvedRoot) || pathEqualFailSafe(resolvedRoot, cleanHome) || pathEqualFailSafe(resolvedRoot, scionHomeDir) {
			// The caller supplied a root, but it is itself one of the paths
			// this function refuses as a source ('/', $HOME, or ~/.scion
			// itself — the last of these reachable when the project
			// directory this root was derived from is, unusually, a git
			// work tree rooted at ~/.scion). Treat that as a
			// misconfiguration and refuse the whole call, rather than
			// letting containment succeed trivially against a root that
			// should never have been usable in the first place. (The
			// ~/.scion floor above would also catch most sources this
			// would otherwise wave through, but a root supplied on its own
			// -- with no source-side overlap to catch -- still needs this.)
			return "", fmt.Errorf("%s source %q is outside the permitted %s root", kind, source, kind)
		}
		usableRoots = append(usableRoots, resolvedRoot)
	}

	if len(usableRoots) > 0 {
		for _, resolvedRoot := range usableRoots {
			if resolvedSource == resolvedRoot || strings.HasPrefix(resolvedSource, resolvedRoot+string(filepath.Separator)) {
				return resolvedSource, nil
			}
		}
		return "", fmt.Errorf("%s source %q is outside the permitted %s root", kind, source, kind)
	}

	// No usable root available: the universal '/', home-directory, and
	// ~/.scion floors above already ran and already refuse anything under
	// ~/.scion that isn't in the named allow list. A source outside
	// ~/.scion entirely, with no root to check it against, is accepted here
	// -- this is the documented fallback for call sites that cannot derive
	// a per-project root at all.
	return resolvedSource, nil
}

// isScionHomeAllowedSubtree reports whether resolvedSource — already
// confirmed to be an exact, case-preserving match for scionHomeDir itself or
// a descendant of it — falls under one of the workspace-bearing subtrees of
// the scion home directory (~/.scion) this guard admits, whether or not the
// call has any roots of its own to check against, because they are real,
// currently-supported workspace locations:
//   - ~/.scion/workspace, or anything under it — the global project's own
//     workspace (pkg/agent/provision.go, ProvisionAgent's Case 3 "global"
//     branch). The whole subtree is admitted; there is nothing further to
//     distinguish within it.
//   - ~/.scion/projects/<slug>/... — see isAllowedProjectSubtree for the
//     shapes admitted within a single hub-managed project's own directory.
//     ~/.scion/projects on its own is NOT admitted — it spans every
//     hub-managed project on the machine, so a specific project directory
//     below it is required.
//   - ~/.scion/project-configs/<dir>/... — see isAllowedProjectConfigsSubtree
//     for the shape admitted within a single hub-managed project's own
//     externalized configuration directory. ~/.scion/project-configs on
//     its own is NOT admitted, for the same reason as ~/.scion/projects.
//
// This is deliberately named and narrow, not a heuristic: anything else
// under ~/.scion (settings, harness-configs, templates, credentials, or
// another project's own subtree that a caller has no root to distinguish)
// stays refused.
func isScionHomeAllowedSubtree(resolvedSource, scionHomeDir string) bool {
	rel, err := filepath.Rel(scionHomeDir, resolvedSource)
	if err != nil {
		return false
	}
	if rel == "." {
		// resolvedSource == scionHomeDir itself: ~/.scion as a whole is
		// never on the allow list, only named descendants of it.
		return false
	}
	parts := strings.SplitN(rel, string(filepath.Separator), 2)
	switch parts[0] {
	case "workspace":
		// ~/.scion/workspace itself, or anything under it: the global
		// project owns the whole subtree, there is nothing further to
		// distinguish within it.
		return true
	case projectkeys.ProjectsDir:
		// ~/.scion/projects on its own spans every hub-managed project;
		// require a specific project directory below it (the second path
		// component from SplitN, non-empty).
		return len(parts) > 1 && parts[1] != "" && isAllowedProjectSubtree(parts[1])
	case projectkeys.ProjectConfigsDir:
		// ~/.scion/project-configs on its own spans every hub-managed
		// project; require a specific project directory below it, the same
		// way as ~/.scion/projects. A pre-rename spelling of this directory
		// is handled either way, not by a dual-name allowance here: after a
		// successful config.MigrateLegacyGlobalLayout, the legacy name is a
		// symlink that resolves through to this canonical directory before
		// resolvedSource is ever compared, so it reaches this same case;
		// if migration was skipped or conflicted, the legacy name is left
		// as a real, separate directory that this switch's default case
		// refuses, fail-closed -- it is deliberately never admitted under
		// its own name.
		return len(parts) > 1 && parts[1] != "" && isAllowedProjectConfigsSubtree(parts[1])
	default:
		return false
	}
}

// isAllowedProjectSubtree reports whether rel — the path under
// ~/.scion/projects/, i.e. "<slug>" or "<slug>/..." — is one of the
// currently-real shapes a single hub-managed project's own directory
// legitimately produces as a workspace source, and refuses everything
// else under that project's directory, named or not:
//   - "<slug>" itself, with nothing further: the whole project directory is
//     the workspace target for a non-git project bootstrapped from GCS
//     (pkg/runtimebroker/handlers.go's workspaceDir, set to
//     filepath.Join(globalDir, "projects", slug) with no further subpath),
//     and for a shared, non-worktree layout (workspace_backend_local.go's
//     HostPath = ProjectDir). Because the whole directory is admitted at
//     this leaf, its "<slug>/..." descendants — including
//     .scion/agents/<agent-id>/home — are reachable too whenever a caller
//     actually uses the bare "<slug>" root; this is by design for those two
//     shapes, not a gap the two more specific rules below are meant to
//     close. agents/ must be excluded from what the mount layer actually
//     shares (e.g. gitignored for a git-backed layout at this path) if that
//     matters for a given deployment.
//   - "<slug>/workspace", or anything under it: the project's externalized
//     workspace, mirroring the global project's own ~/.scion/workspace.
//   - "<slug>/.scion/agents/<agent-id>/workspace", or anything under it: a
//     single agent's own per-agent worktree/workspace
//     (pkg/agent/provision.go's agentWorkspace, filepath.Join(agentDir,
//     "workspace"), where agentDir is this project's own
//     .scion/agents/<agent-id>).
//   - "<slug>/worktrees/<name>", or anything under it: the hub-native
//     worktree-per-agent shared-base layout, where the project's own
//     directory is the shared git checkout and each agent's worktree is a
//     direct child of its "worktrees" subdirectory. Modeled on the agents
//     rule above (a variable name segment, not a fixed leaf like
//     "workspace"): the implementation CutPrefixes the fixed "worktrees/"
//     literal, then requires the remaining first path segment to be
//     non-empty, so a bare "worktrees" (no match, no trailing separator) or
//     an empty name ("worktrees/" alone) are both rejected rather than
//     admitting the "worktrees" directory itself.
//
// The last three rules exist for callers that resolve a source narrower than
// the bare "<slug>" directory — resolveProjectRoot's parent-of-.scion
// fallback, for example, which lands exactly on "<slug>/.scion" not
// "<slug>" — and so cannot rely on the first rule to admit an agent's own
// workspace without also, at that narrower scope, admitting its home.
func isAllowedProjectSubtree(rel string) bool {
	slugParts := strings.SplitN(rel, string(filepath.Separator), 2)
	if len(slugParts) == 1 || slugParts[1] == "" {
		return true // "<slug>" itself.
	}
	remainder := slugParts[1]
	if remainder == "workspace" || strings.HasPrefix(remainder, "workspace"+string(filepath.Separator)) {
		return true
	}
	agentsPrefix := filepath.Join(".scion", "agents") + string(filepath.Separator)
	if after, ok := strings.CutPrefix(remainder, agentsPrefix); ok {
		agentParts := strings.SplitN(after, string(filepath.Separator), 2)
		if len(agentParts) == 2 && (agentParts[1] == "workspace" || strings.HasPrefix(agentParts[1], "workspace"+string(filepath.Separator))) {
			return true
		}
	}
	// "<slug>/worktrees/<name>" or "<slug>/worktrees/<name>/..." — see this
	// function's doc comment.
	worktreesPrefix := "worktrees" + string(filepath.Separator)
	if after, ok := strings.CutPrefix(remainder, worktreesPrefix); ok {
		nameParts := strings.SplitN(after, string(filepath.Separator), 2)
		if nameParts[0] != "" {
			return true
		}
	}
	return false
}

// isAllowedProjectConfigsSubtree reports whether rel — the path under
// ~/.scion/project-configs/, i.e. "<dir>" or "<dir>/..." — is the one
// currently-real shape a hub-dispatched project's own externalized
// directory produces as a workspace source: "<dir>/.scion/agents/<agent-id>/workspace",
// or anything under it. Two provision.go branches produce it: the GitClone
// branch (agentWorkspace = filepath.Join(agentDir, "workspace")) and the
// non-git, marker-resolved branch (isProjectConfigsPath), both once
// config.ResolveProjectPath has substituted this externalized directory,
// via the project's marker file, for the project's nominal
// ~/.scion/projects/<slug>/.scion, so agentDir in both cases is this
// project's own .scion/agents/<agent-id>. Unlike ~/.scion/projects/<slug>,
// the bare "<dir>" itself is NOT admitted here: project-configs/<dir>
// itself holds the project's externalized configuration, not a workspace,
// so there is no equivalent to isAllowedProjectSubtree's bare-"<slug>"
// shape to admit at this leaf.
func isAllowedProjectConfigsSubtree(rel string) bool {
	dirParts := strings.SplitN(rel, string(filepath.Separator), 2)
	if len(dirParts) == 1 || dirParts[1] == "" {
		return false // "<dir>" itself: not a workspace, refused.
	}
	remainder := dirParts[1]
	agentsPrefix := filepath.Join(".scion", "agents") + string(filepath.Separator)
	if after, ok := strings.CutPrefix(remainder, agentsPrefix); ok {
		agentParts := strings.SplitN(after, string(filepath.Separator), 2)
		if len(agentParts) == 2 && (agentParts[1] == "workspace" || strings.HasPrefix(agentParts[1], "workspace"+string(filepath.Separator))) {
			return true
		}
	}
	return false
}

// isScionHomeAllowedHomeSubtree is isScionHomeAllowedSubtree's counterpart
// for ValidateAgentHomeSource: it reports whether resolvedSource — already
// confirmed to be an exact, case-preserving match for scionHomeDir itself or
// a descendant of it — falls under one of the three real shapes
// config.GetAgentHomePath produces for an agent's own home directory under
// ~/.scion, for each of the three project layouts that keep an agent's home
// there:
//   - ~/.scion/agents/<agent-id>/home, or anything under it — the global
//     project's own agent homes (GetAgentHomePath's fallback branch, called
//     with ~/.scion itself as projectDir).
//   - ~/.scion/projects/<slug>/.scion/agents/<agent-id>/home, or anything
//     under it — a hub-managed project's agent homes.
//   - ~/.scion/project-configs/<dir>/.scion/agents/<agent-id>/home, or
//     anything under it — an externalized git project's agent homes (see
//     GetGitProjectExternalAgentsDir).
//
// None of these name a "workspace" leaf, and nothing isScionHomeAllowedSubtree
// admits names a "home" leaf: the two allow lists are disjoint by
// construction, not merely by convention, so a workspace can never pass as a
// home or vice versa under ~/.scion.
func isScionHomeAllowedHomeSubtree(resolvedSource, scionHomeDir string) bool {
	rel, err := filepath.Rel(scionHomeDir, resolvedSource)
	if err != nil {
		return false
	}
	if rel == "." {
		// resolvedSource == scionHomeDir itself: never a home on its own.
		return false
	}
	parts := strings.SplitN(rel, string(filepath.Separator), 2)
	switch parts[0] {
	case "agents":
		// ~/.scion/agents/<agent-id>/home, or anything under it: the global
		// project's own agent home, with nothing further to distinguish
		// within it.
		return len(parts) > 1 && isAgentHomeLeaf(parts[1])
	case projectkeys.ProjectsDir:
		return len(parts) > 1 && parts[1] != "" && isAllowedProjectHomeSubtree(parts[1])
	case projectkeys.ProjectConfigsDir:
		return len(parts) > 1 && parts[1] != "" && isAllowedProjectConfigsHomeSubtree(parts[1])
	default:
		return false
	}
}

// isAgentHomeLeaf reports whether rel — the path under
// ~/.scion/agents/, i.e. "<agent-id>" or "<agent-id>/..." — names a
// particular agent's "home" leaf, or anything under it.
func isAgentHomeLeaf(rel string) bool {
	agentParts := strings.SplitN(rel, string(filepath.Separator), 2)
	return len(agentParts) == 2 && (agentParts[1] == "home" || strings.HasPrefix(agentParts[1], "home"+string(filepath.Separator)))
}

// isAllowedProjectHomeSubtree is isAllowedProjectSubtree's counterpart for
// agent homes: it reports whether rel — the path under
// ~/.scion/projects/, i.e. "<slug>" or "<slug>/..." — names
// "<slug>/.scion/agents/<agent-id>/home", or anything under it. Unlike
// isAllowedProjectSubtree, the bare "<slug>" directory is NOT admitted here:
// it is a hub-managed project's own directory, not any single agent's home,
// so there is no equivalent to isAllowedProjectSubtree's bare-"<slug>" shape
// to admit at this leaf.
func isAllowedProjectHomeSubtree(rel string) bool {
	slugParts := strings.SplitN(rel, string(filepath.Separator), 2)
	if len(slugParts) == 1 || slugParts[1] == "" {
		return false // "<slug>" itself: not a home, refused.
	}
	remainder := slugParts[1]
	agentsPrefix := filepath.Join(".scion", "agents") + string(filepath.Separator)
	if after, ok := strings.CutPrefix(remainder, agentsPrefix); ok {
		return isAgentHomeLeaf(after)
	}
	return false
}

// isAllowedProjectConfigsHomeSubtree is isAllowedProjectConfigsSubtree's
// counterpart for agent homes: it reports whether rel — the path under
// ~/.scion/project-configs/, i.e. "<dir>" or "<dir>/..." — names
// "<dir>/.scion/agents/<agent-id>/home", or anything under it, mirroring
// GetGitProjectExternalAgentsDir's layout for an externalized git project's
// agent homes.
func isAllowedProjectConfigsHomeSubtree(rel string) bool {
	dirParts := strings.SplitN(rel, string(filepath.Separator), 2)
	if len(dirParts) == 1 || dirParts[1] == "" {
		return false // "<dir>" itself: not a home, refused.
	}
	remainder := dirParts[1]
	agentsPrefix := filepath.Join(".scion", "agents") + string(filepath.Separator)
	if after, ok := strings.CutPrefix(remainder, agentsPrefix); ok {
		return isAgentHomeLeaf(after)
	}
	return false
}

// caseInsensitiveFilesystem reports whether the current platform's default
// filesystem is case-insensitive (macOS's APFS/HFS+, Windows' NTFS/FAT).
// This is a coarse, platform-level check by spelling only, not a per-volume,
// file-identity one, and it is not exhaustive in either direction:
//   - A case-sensitive volume can be mounted on darwin or windows, in which
//     case this over-reports insensitivity. That only causes extra,
//     unnecessary refusals from the case-fold comparisons it gates
//     (pathEqualFailSafe, pathIsOrUnderCaseFold) — never a missed one.
//   - A case-insensitive volume is not impossible on Linux (uncommon
//     filesystems, case-insensitive overlays, or a macOS filesystem
//     accessed via a case-insensitive network share), in which case this
//     under-reports insensitivity, and the case-fold comparisons are
//     skipped entirely: a spelling variant of ~/.scion or $HOME that the
//     filesystem treats as the same file would then compare as different
//     paths and not match the floors it is meant to close. This is a real,
//     accepted limitation of a spelling-based check — closing it fully
//     would need a file-identity comparison (e.g. os.SameFile against a
//     stat of the real ancestor), which this guard does not attempt.
//
// This is a variable, not a plain function, precisely so a test can
// override it directly to exercise the case-insensitive path on any host,
// including CI (Linux, where the platform default is case-sensitive and
// this variable's initial value is always false).
var caseInsensitiveFilesystem = runtime.GOOS == "darwin" || runtime.GOOS == "windows"

// pathEqualFailSafe reports whether a and b name the same path, for a DENY
// decision: an exact, case-preserving match always counts, and on a
// platform with a case-insensitive default filesystem, a case-insensitive
// match counts too. This is deliberately one-directional — broadening only
// which paths this function says are equal, never broadening what an equal
// result is used for — so it must only gate refusals (a root or source
// equal to $HOME or ~/.scion), never grant acceptance: denying a few extra,
// merely-case-variant paths on a case-sensitive filesystem is a safe false
// positive, but treating a case-variant path as equal for an ACCEPT
// decision would not be.
func pathEqualFailSafe(a, b string) bool {
	return pathEqualFailSafeCaseFold(a, b, caseInsensitiveFilesystem)
}

// pathEqualFailSafeCaseFold is pathEqualFailSafe with the platform check
// factored out to a parameter, so the comparison logic itself can be unit
// tested on any host regardless of that host's own filesystem semantics.
func pathEqualFailSafeCaseFold(a, b string, caseInsensitive bool) bool {
	if a == b {
		return true
	}
	return caseInsensitive && strings.EqualFold(a, b)
}

// pathIsOrUnderCaseFold reports whether resolvedPath is a case-insensitive
// match for ancestor itself, or for ancestor plus a path separator and any
// suffix — the case-insensitive counterpart to the exact
// "resolvedPath == ancestor || strings.HasPrefix(resolvedPath,
// ancestor+separator)" check callers run first. Like pathEqualFailSafe, this
// exists only to broaden a DENY decision (recognizing that a case-variant
// spelling is, on a case-insensitive filesystem, the very same file as
// ancestor or one of its descendants) and must never be used to grant
// acceptance on its own.
func pathIsOrUnderCaseFold(resolvedPath, ancestor string) bool {
	return pathIsOrUnderCaseFoldForOS(resolvedPath, ancestor, caseInsensitiveFilesystem)
}

// pathIsOrUnderCaseFoldForOS is pathIsOrUnderCaseFold with the platform
// check factored out to a parameter, so the comparison logic itself can be
// unit tested on any host regardless of that host's own filesystem
// semantics.
func pathIsOrUnderCaseFoldForOS(resolvedPath, ancestor string, caseInsensitive bool) bool {
	if !caseInsensitive {
		return false
	}
	lowerPath := strings.ToLower(resolvedPath)
	lowerAncestor := strings.ToLower(ancestor)
	return lowerPath == lowerAncestor || strings.HasPrefix(lowerPath, lowerAncestor+string(filepath.Separator))
}

// resolveForValidation returns the real, symlink-free form of path, for
// comparisons that must judge a path by where it actually leads rather than
// its nominal spelling. If path does not exist yet (e.g. a value about to be
// bootstrapped, or a caller validating ahead of directory creation), it
// resolves the nearest existing ancestor directory and rejoins the
// not-yet-created remainder onto that resolved ancestor, so a legitimate
// not-yet-created path still gets a concrete, symlink-resolved answer instead
// of being rejected merely for not existing yet.
//
// A dangling symlink — a path component that exists as a directory entry but
// whose target does not — is not treated as "not yet created": os.Lstat
// (which does not follow symlinks) is used to tell the two apart before a
// component is pushed onto the not-yet-created suffix, and a dangling link
// fails closed rather than being silently resolved as if it were an ordinary
// future path.
//
// Any other resolution failure — including reaching the top of the path
// without finding an existing ancestor — is returned as an error, for the
// caller to treat as a denial.
//
// This function only ever proves what the path currently resolves to. It
// cannot protect against a not-yet-created suffix being replaced with a
// symlink between this call returning and the caller actually creating or
// using that path — callers that create-then-use a not-yet-existing result
// document that residual gap at their own call site.
func resolveForValidation(path string) (string, error) {
	cleaned := filepath.Clean(path)

	resolved, err := filepath.EvalSymlinks(cleaned)
	if err == nil {
		return resolved, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}

	dir := cleaned
	var suffix []string
	for {
		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached the top of the path without finding an existing
			// ancestor to resolve against.
			return "", err
		}

		// Before treating dir as "not yet created," confirm it truly does
		// not exist at all. EvalSymlinks also fails with IsNotExist for a
		// dangling symlink: dir itself exists as a directory entry, only its
		// target doesn't. Lstat distinguishes the two.
		if _, lstatErr := os.Lstat(dir); lstatErr == nil {
			return "", fmt.Errorf("cannot resolve %s: broken symlink", dir)
		} else if !os.IsNotExist(lstatErr) {
			return "", lstatErr
		}

		suffix = append([]string{filepath.Base(dir)}, suffix...)
		dir = parent

		resolvedDir, dirErr := filepath.EvalSymlinks(dir)
		if dirErr == nil {
			return filepath.Join(append([]string{resolvedDir}, suffix...)...), nil
		}
		if !os.IsNotExist(dirErr) {
			return "", dirErr
		}
	}
}

// isFilesystemRoot reports whether p, already resolved and cleaned, names
// the top of its filesystem: filepath.Dir of a root returns the root itself
// on every OS this project supports, including a Windows volume root such as
// `C:\`, which a literal comparison against the single-separator string "/"
// (true only for the Unix root) would miss entirely.
func isFilesystemRoot(p string) bool {
	return filepath.Dir(p) == p
}
