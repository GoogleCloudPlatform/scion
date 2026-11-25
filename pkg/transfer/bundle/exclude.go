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

package bundle

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// defaultCaseInsensitiveFS reports whether the host OS commonly mounts agent
// directories on a case-insensitive filesystem (macOS APFS/HFS+ for Apple
// Container / Docker Desktop, and Windows NTFS). On such hosts an in-container
// tool can create or open `.Docker/config.json` through a case-insensitive
// bind mount while the host directory walk yields `.Docker/config.json`, which
// would bypass case-sensitive exclusion matching.
func defaultCaseInsensitiveFS() bool {
	return runtime.GOOS == "darwin" || runtime.GOOS == "windows"
}

// Excluder reports whether a slash-separated root-relative path should be
// omitted from a bundle. When isDir is true and Exclude returns true, the
// walker skips the entire directory subtree.
type Excluder interface {
	Exclude(relPath string, isDir bool) bool
}

// ExcluderFunc adapts a function to Excluder.
type ExcluderFunc func(relPath string, isDir bool) bool

// Exclude calls f(relPath, isDir).
func (f ExcluderFunc) Exclude(relPath string, isDir bool) bool {
	return f(relPath, isDir)
}

// rootResolvableExcluder is implemented by path-based excluders so Write and
// Estimate can rebase exclusion entries through any in-root symlinked ancestor
// directories (failing closed with *ErrUnportableHome on unresolvable ancestor
// symlinks) and collect hard-linked excluded secret file identities before walking.
type rootResolvableExcluder interface {
	resolveInRoot(root *os.Root) (Excluder, excludedInodeSet, error)
}

func resolveExcluderInRoot(root *os.Root, ex Excluder) (Excluder, excludedInodeSet, error) {
	if r, ok := ex.(rootResolvableExcluder); ok {
		return r.resolveInRoot(root)
	}
	return ex, excludedInodeSet{}, nil
}

const homeHarnessOutputsRel = ".scion/harness/outputs"

// HomeBaselineExcludes are the static Scion-managed secret and runtime-state
// paths always excluded from a portable $HOME bundle (design §3.4 items 1–2),
// even when HomeExcluder is called with zero options. Kept in sync with
// runtime.PortableHomeExcludes by a parity test in pkg/runtime.
var HomeBaselineExcludes = []string{
	".scion/scion-token",
	".scion/scion-token.tmp",
	".scion/secrets.json",
	".scion/harness/secrets",
	homeHarnessOutputsRel,
	".scion-home-reset-id",
}

// HomeIdentityCredentialExcludes are identity-bound credentials minted for the
// source broker's identity that must never travel in a move bundle (design §3.4
// item 3 / D6).
var HomeIdentityCredentialExcludes = []string{
	".config/gcloud/credentials.db",
	".config/gcloud/access_tokens.db",
	".config/gcloud/application_default_credentials.json",
	".config/gcloud/legacy_credentials/**",
	".docker/config.json",
	".kube/config",
}

// HomeCacheExcludes are regenerable cache directories omitted from a $HOME
// bundle to save bandwidth and space (design §3.4 item 4 / D6).
var HomeCacheExcludes = []string{
	".cache/**",
	".npm/_cacache/**",
	"go/pkg/mod/cache/**",
}

// HomeExcluderOptions configures HomeExcluder.
type HomeExcluderOptions struct {
	// ExtraLiterals are additional home-relative literal paths (such as
	// runtime.PortableHomeExcludes(cfg) for per-agent resolved secrets and auth
	// files), canonicalised via path.Clean (stripping leading "./" and trailing
	// "/") and matched literally as exact path or subtree prefix (never globs).
	// Absolute paths or ".." traversal entries cause Write and Estimate to fail
	// closed with an error.
	ExtraLiterals []string
	// TempNamePred, when non-nil, matches temporary filenames by basename
	// (such as runtime.IsHomeTempName).
	TempNamePred func(base string) bool
	// AgentHomeAbs is the agent's in-container absolute $HOME path (e.g.
	// "/home/scion"). When non-empty, an absolute symlink target under
	// AgentHomeAbs encountered while resolving an exclusion entry is rebased to
	// a root-relative path and resolution continues.
	AgentHomeAbs string
	// CaseInsensitive enables case-insensitive exclusion prefix matching.
	// Always enabled automatically on macOS (APFS/HFS+) and Windows; can be
	// set explicitly in tests or for case-insensitive mounts on Linux.
	CaseInsensitive bool
}

// WorkspaceExcluderOptions configures WorkspaceExcluderWithOptions.
type WorkspaceExcluderOptions struct {
	// ExtraLiterals are additional workspace-relative literal paths (such as
	// file-type secrets or auth files targeted under /workspace), canonicalised
	// via path.Clean and matched literally as exact path or subtree prefix
	// (never globs). Absolute paths or ".." traversal entries cause Write and
	// Estimate to fail closed with an error.
	ExtraLiterals []string
	// WorkspaceAbs is the agent's in-container absolute workspace mount path
	// (typically "/workspace"). When non-empty, an absolute symlink target under
	// WorkspaceAbs encountered while resolving an exclusion entry is rebased to
	// a root-relative path and resolution continues.
	WorkspaceAbs string
	// CaseInsensitive enables case-insensitive exclusion prefix matching for
	// ExtraLiterals (root `.scion` is always matched case-insensitively).
	// Automatically enabled on macOS and Windows.
	CaseInsensitive bool
}

// WorkspaceExcluder returns the excluder for a workspace root: root `.scion`
// and `.scion/**` only (case-insensitive on the root element, matching
// transfer.ExcludeWorkspaceObject). `.git/**` and nested `a/.scion` are kept.
func WorkspaceExcluder() Excluder {
	return WorkspaceExcluderWithOptions(WorkspaceExcluderOptions{})
}

// WorkspaceExcluderWithOptions returns the excluder for a workspace root: root
// `.scion` and `.scion/**` (case-insensitive on the root element) unioned with
// any caller-supplied ExtraLiterals (e.g. file-type secrets or auth files
// targeted under /workspace). When passed to Write or Estimate, ExtraLiterals
// receive the same fail-closed symlinked-ancestor resolution (rebasing absolute
// links under opts.WorkspaceAbs) and excluded-secret hardlink detection as
// HomeExcluder.
func WorkspaceExcluderWithOptions(opts WorkspaceExcluderOptions) Excluder {
	var secretPrefixes []string
	var initErr error
	for _, raw := range opts.ExtraLiterals {
		c, err := canonicalizeExtraLiteral(raw)
		if err != nil {
			if initErr == nil {
				initErr = err
			}
			continue
		}
		if c == "" {
			continue
		}
		secretPrefixes = append(secretPrefixes, c)
	}
	ci := opts.CaseInsensitive || defaultCaseInsensitiveFS()
	return &workspaceExcluder{
		inner: newPrefixExcluder(secretPrefixes, secretPrefixes, nil, nil, cleanAbsHome(opts.WorkspaceAbs), ci, initErr),
	}
}

type workspaceExcluder struct {
	inner *prefixExcluder
}

func (e *workspaceExcluder) Exclude(relPath string, isDir bool) bool {
	first, _, _ := strings.Cut(relPath, "/")
	if transfer.IsWorkspaceIdentityName(first) {
		return true
	}
	return e.inner.Exclude(relPath, isDir)
}

func (e *workspaceExcluder) resolveInRoot(root *os.Root) (Excluder, excludedInodeSet, error) {
	resolved, inodes, err := e.inner.resolveInRoot(root)
	if err != nil {
		return nil, excludedInodeSet{}, err
	}
	return &workspaceExcluder{inner: resolved.(*prefixExcluder)}, inodes, nil
}

// HomeExcluder returns the excluder for a $HOME root: always including the
// built-in static baseline (HomeBaselineExcludes, HomeIdentityCredentialExcludes,
// HomeCacheExcludes) plus any caller-supplied ExtraLiterals and TempNamePred
// (design §3.4 "Exclusions" items 1–4).
//
// When passed to Write or Estimate, every exclusion entry is resolved through
// symlinks before the walk:
//   - Relative links and absolute links under opts.AgentHomeAbs are rebased to
//     their physical root-relative path and added to the exclusion set.
//   - If an ancestor component of any exclusion entry is a symlink that cannot
//     be resolved inside the root (absolute outside AgentHomeAbs, relative
//     escaping, loop, or too many hops), Write and Estimate fail closed with
//     *ErrUnportableHome.
//   - On Unix, hard-linked regular files matching excluded secret files (and
//     regular files under excluded credential directories, up to
//     maxExcludedHardlinkScanEntries) are skipped and counted as Excluded;
//     caches and .scion/harness/outputs are excluded lexically only so user
//     files hard-linked into caches (e.g. uv venvs) are preserved.
func HomeExcluder(opts HomeExcluderOptions) Excluder {
	// Put credential directories first so the hardlink scan prioritises them.
	secretPrefixes := make([]string, 0, len(HomeIdentityCredentialExcludes)+len(HomeBaselineExcludes)+len(opts.ExtraLiterals))
	nonSecretPrefixes := make([]string, 0, 1+len(HomeCacheExcludes))

	for _, pat := range HomeIdentityCredentialExcludes {
		if p := normalizePrefixPattern(pat); p != "" {
			secretPrefixes = append(secretPrefixes, p)
		}
	}
	for _, p := range HomeBaselineExcludes {
		if p == homeHarnessOutputsRel {
			nonSecretPrefixes = append(nonSecretPrefixes, p)
			continue
		}
		secretPrefixes = append(secretPrefixes, p)
	}
	var initErr error
	for _, raw := range opts.ExtraLiterals {
		c, err := canonicalizeExtraLiteral(raw)
		if err != nil {
			if initErr == nil {
				initErr = err
			}
			continue
		}
		if c == "" {
			continue
		}
		if c == homeHarnessOutputsRel {
			nonSecretPrefixes = append(nonSecretPrefixes, c)
			continue
		}
		secretPrefixes = append(secretPrefixes, c)
	}
	for _, pat := range HomeCacheExcludes {
		if p := normalizePrefixPattern(pat); p != "" {
			nonSecretPrefixes = append(nonSecretPrefixes, p)
		}
	}

	allPrefixes := make([]string, 0, len(secretPrefixes)+len(nonSecretPrefixes))
	allPrefixes = append(allPrefixes, secretPrefixes...)
	allPrefixes = append(allPrefixes, nonSecretPrefixes...)

	ci := opts.CaseInsensitive || defaultCaseInsensitiveFS()
	return newPrefixExcluder(allPrefixes, secretPrefixes, nonSecretPrefixes, opts.TempNamePred, cleanAbsHome(opts.AgentHomeAbs), ci, initErr)
}

// PatternExcluder builds an Excluder from slash-separated relative patterns.
//
// Supported forms (no glob metacharacter parsing, so paths containing
// brackets or wildcards such as ".config/app[1]/token" match literally and
// never fail open):
//   - "prefix/**" matches "prefix" itself and any descendant "prefix/...".
//   - Any other pattern is matched literally as an exact path or subtree
//     prefix ("a/b" matches "a/b" and "a/b/...").
func PatternExcluder(patterns []string) Excluder {
	prefixes := make([]string, 0, len(patterns))
	for _, pat := range patterns {
		if p := normalizePrefixPattern(pat); p != "" {
			prefixes = append(prefixes, p)
		}
	}
	return &prefixExcluder{
		prefixes:          prefixes,
		nonSecretPrefixes: prefixes,
	}
}

func normalizePrefixPattern(pat string) string {
	pat = strings.TrimSpace(pat)
	if pat == "" {
		return ""
	}
	if prefix, ok := strings.CutSuffix(pat, "/**"); ok {
		return prefix
	}
	return pat
}

func canonicalizeExtraLiteral(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	if strings.ContainsRune(s, 0) || strings.Contains(s, `\`) {
		return "", fmt.Errorf("invalid home exclude literal %q: contains NUL or backslash", raw)
	}
	if path.IsAbs(s) || filepath.IsAbs(s) {
		return "", fmt.Errorf("invalid home exclude literal %q: must be relative", raw)
	}
	c := path.Clean(s)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("invalid home exclude literal %q: escapes or names root", raw)
	}
	if err := transfer.ValidateRelPath(c); err != nil {
		return "", fmt.Errorf("invalid home exclude literal %q: %w", raw, err)
	}
	return c, nil
}

func cleanAbsHome(homeAbs string) string {
	homeAbs = strings.TrimSpace(homeAbs)
	if homeAbs == "" {
		return ""
	}
	homeAbs = path.Clean(homeAbs)
	if !strings.HasPrefix(homeAbs, "/") || homeAbs == "/" {
		return ""
	}
	return homeAbs
}

type prefixExcluder struct {
	prefixes          []string
	lowerPrefixes     []string
	secretPrefixes    []string
	nonSecretPrefixes []string
	tempNamePred      func(base string) bool
	agentHomeAbs      string
	caseInsensitive   bool
	initErr           error
}

func newPrefixExcluder(prefixes, secretPrefixes, nonSecretPrefixes []string, tempNamePred func(base string) bool, agentHomeAbs string, caseInsensitive bool, initErr error) *prefixExcluder {
	var lower []string
	if caseInsensitive && len(prefixes) > 0 {
		lower = make([]string, len(prefixes))
		for i, p := range prefixes {
			lower[i] = strings.ToLower(p)
		}
	}
	return &prefixExcluder{
		prefixes:          prefixes,
		lowerPrefixes:     lower,
		secretPrefixes:    secretPrefixes,
		nonSecretPrefixes: nonSecretPrefixes,
		tempNamePred:      tempNamePred,
		agentHomeAbs:      agentHomeAbs,
		caseInsensitive:   caseInsensitive,
		initErr:           initErr,
	}
}

func (e *prefixExcluder) Exclude(relPath string, _ bool) bool {
	if e.tempNamePred != nil && e.tempNamePred(path.Base(relPath)) {
		return true
	}
	if e.caseInsensitive {
		// Note: strings.ToLower performs ASCII/simple Unicode case folding, not
		// full APFS Unicode case-folding + NFD normalization. All built-in
		// exclusion prefixes are ASCII, so this covers every standard credential
		// and cache path; non-ASCII user secret targets remain best-effort.
		lp := strings.ToLower(relPath)
		for _, p := range e.lowerPrefixes {
			if lp == p || strings.HasPrefix(lp, p+"/") {
				return true
			}
		}
		return false
	}
	for _, p := range e.prefixes {
		if relPath == p || strings.HasPrefix(relPath, p+"/") {
			return true
		}
	}
	return false
}

// resolveInRoot expands each exclusion prefix through symlinks (failing closed
// with *ErrUnportableHome if an ancestor symlink cannot be resolved inside
// root) and collects the hard-linked excluded secret inode set on Unix (from
// secret prefixes only, never from caches or .scion/harness/outputs).
func (e *prefixExcluder) resolveInRoot(root *os.Root) (Excluder, excludedInodeSet, error) {
	if e.initErr != nil {
		return nil, excludedInodeSet{}, e.initErr
	}
	resolveList := func(in []string) ([]string, error) {
		seen := make(map[string]struct{}, len(in))
		out := make([]string, 0, len(in))
		add := func(p string) {
			if p == "" {
				return
			}
			if _, dup := seen[p]; dup {
				return
			}
			seen[p] = struct{}{}
			out = append(out, p)
		}
		for _, p := range in {
			add(p)
			phys, err := resolveExclusionPathInRoot(root, p, e.agentHomeAbs)
			if err != nil {
				return nil, err
			}
			if phys != "" {
				add(phys)
			}
		}
		return out, nil
	}

	resolvedSecrets, err := resolveList(e.secretPrefixes)
	if err != nil {
		return nil, excludedInodeSet{}, err
	}
	resolvedNonSecrets, err := resolveList(e.nonSecretPrefixes)
	if err != nil {
		return nil, excludedInodeSet{}, err
	}

	inodes, err := collectExcludedInodes(root, resolvedSecrets)
	if err != nil {
		return nil, excludedInodeSet{}, err
	}

	all := make([]string, 0, len(resolvedSecrets)+len(resolvedNonSecrets))
	seenAll := make(map[string]struct{}, len(resolvedSecrets)+len(resolvedNonSecrets))
	for _, list := range [][]string{resolvedSecrets, resolvedNonSecrets} {
		for _, p := range list {
			if _, dup := seenAll[p]; dup {
				continue
			}
			seenAll[p] = struct{}{}
			all = append(all, p)
		}
	}

	return newPrefixExcluder(all, resolvedSecrets, resolvedNonSecrets, e.tempNamePred, e.agentHomeAbs, e.caseInsensitive, nil), inodes, nil
}

// resolveExclusionPathInRoot resolves exPath component by component through
// root, rebasing absolute symlink targets under agentHomeAbs to root-relative
// paths. If any ancestor component is a symlink that cannot be resolved inside
// root (absolute outside agentHomeAbs, relative escaping, loop, or too many
// hops), it returns *ErrUnportableHome. A leaf symlink pointing outside the
// root (such as a K8s NFS-home /scion-staging/... secret link) cannot expose a
// physical copy inside root, so it returns ("", nil) and remains excluded by
// its lexical path.
func resolveExclusionPathInRoot(root *os.Root, exPath, agentHomeAbs string) (string, error) {
	var cur []string
	queue := strings.Split(exPath, "/")
	hops := 0
	lastSymlink := ""
	inAncestor := false

	for len(queue) > 0 {
		comp := queue[0]
		queue = queue[1:]

		switch comp {
		case "", ".":
			continue
		case "..":
			if len(cur) == 0 {
				if inAncestor {
					return "", &ErrUnportableHome{
						Path:   lastSymlink,
						Reason: "symlink escapes home root",
					}
				}
				return "", nil
			}
			cur = cur[:len(cur)-1]
			continue
		}

		cur = append(cur, comp)
		cand := strings.Join(cur, "/")
		fi, err := root.Lstat(filepath.FromSlash(cand))
		if err != nil {
			continue
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			continue
		}

		isAncestor := len(queue) > 0
		if isAncestor {
			inAncestor = true
		}
		lastSymlink = cand

		hops++
		if hops > maxSymlinkHops {
			if inAncestor {
				return "", &ErrUnportableHome{
					Path:   cand,
					Reason: "symlink loop or too many hops",
				}
			}
			return "", nil
		}

		linkTarget, err := root.Readlink(filepath.FromSlash(cand))
		if err != nil {
			if inAncestor {
				return "", &ErrUnportableHome{
					Path:   cand,
					Reason: "cannot read symlink",
				}
			}
			return "", nil
		}
		if linkTarget == "" || strings.ContainsRune(linkTarget, 0) || strings.Contains(linkTarget, `\`) {
			if inAncestor {
				return "", &ErrUnportableHome{
					Path:   cand,
					Reason: "invalid symlink target",
				}
			}
			return "", nil
		}

		if path.IsAbs(linkTarget) || filepath.IsAbs(linkTarget) {
			if agentHomeAbs != "" {
				cleanTarget := path.Clean(linkTarget)
				if cleanTarget == agentHomeAbs {
					cur = nil
					continue
				}
				if rebased, ok := strings.CutPrefix(cleanTarget, agentHomeAbs+"/"); ok {
					cur = nil
					queue = append(strings.Split(rebased, "/"), queue...)
					continue
				}
			}
			if inAncestor {
				return "", &ErrUnportableHome{
					Path:   cand,
					Reason: "absolute symlink outside home",
				}
			}
			return "", nil
		}

		// Relative symlink target: resolve relative to cand's parent directory.
		cur = cur[:len(cur)-1]
		queue = append(strings.Split(linkTarget, "/"), queue...)
	}
	return strings.Join(cur, "/"), nil
}
