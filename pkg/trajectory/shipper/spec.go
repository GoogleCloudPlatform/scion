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

package shipper

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/util/pathcanon"
)

// ArtifactKind describes how a native harness artifact is captured.
type ArtifactKind string

const (
	// ArtifactKindJSONLAppend identifies append-only JSONL files captured as
	// byte-range segments.
	ArtifactKindJSONLAppend ArtifactKind = "jsonl_append"
)

// MaxNativeRelativePathBytes is the wire-contract upper bound on a
// native_relative_path.
const MaxNativeRelativePathBytes = 1024

// ArtifactRule defines a home-relative directory root and file suffix to
// discover for a harness.
type ArtifactRule struct {
	// Root is a home-relative directory path using '/' separators with no
	// leading or trailing slash (for example ".claude/projects").
	Root string
	// Suffix is the required filename suffix (for example ".jsonl").
	Suffix string
	// Kind is the artifact capture kind (for example ArtifactKindJSONLAppend).
	Kind ArtifactKind
}

// HarnessSpec defines the allow-listed artifact discovery rules for a harness.
type HarnessSpec struct {
	Name  string
	Rules []ArtifactRule
}

// ClaudeHarnessSpec returns the built-in discovery specification for Claude
// Code session and subagent JSONL transcripts.
func ClaudeHarnessSpec() HarnessSpec {
	return HarnessSpec{
		Name: "claude",
		Rules: []ArtifactRule{
			{
				Root:   ".claude/projects",
				Suffix: ".jsonl",
				Kind:   ArtifactKindJSONLAppend,
			},
		},
	}
}

// HarnessSpecFor returns the built-in HarnessSpec for the named harness.
func HarnessSpecFor(name string) (HarnessSpec, bool) {
	switch name {
	case "claude":
		return ClaudeHarnessSpec(), true
	default:
		return HarnessSpec{}, false
	}
}

// DiscoveredArtifact is a validated artifact file located under an agent home.
type DiscoveredArtifact struct {
	NativeRelativePath string
	AbsPath            string
	RuleRootAbs        string
	RelWithinRuleRoot  string
	Kind               ArtifactKind
	SessionID          string
}

// RefusedPath records a path skipped during discovery due to a safety check
// (such as a symlink escaping the rule root or an invalid relative path).
type RefusedPath struct {
	NativeRelativePath string
	Reason             string
}

// DiscoveryResult contains all validated artifacts and any refused paths found
// during a scan of an agent home.
type DiscoveryResult struct {
	Artifacts []DiscoveredArtifact
	Refused   []RefusedPath
}

// ValidateNativeRelativePath validates a candidate native_relative_path against
// the wire contract and the harness's allow-listed roots. On success it returns
// the matching ArtifactKind.
func ValidateNativeRelativePath(rel string, spec HarnessSpec) (ArtifactKind, error) {
	if rel == "" {
		return "", errors.New("native_relative_path is empty")
	}
	if len(rel) > MaxNativeRelativePathBytes {
		return "", fmt.Errorf("native_relative_path exceeds %d bytes", MaxNativeRelativePathBytes)
	}
	if strings.HasPrefix(rel, "/") {
		return "", errors.New("native_relative_path must be relative")
	}
	if strings.ContainsRune(rel, '\\') {
		return "", errors.New("native_relative_path must not contain backslashes")
	}
	for i := 0; i < len(rel); i++ {
		b := rel[i]
		if b < 0x20 || b == 0x7f {
			return "", errors.New("native_relative_path must not contain control characters")
		}
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("native_relative_path contains invalid segment %q", seg)
		}
	}

	for _, rule := range spec.Rules {
		root := strings.Trim(rule.Root, "/")
		if root == "" {
			continue
		}
		if strings.HasPrefix(rel, root+"/") && (rule.Suffix == "" || strings.HasSuffix(rel, rule.Suffix)) {
			return rule.Kind, nil
		}
	}
	return "", fmt.Errorf("native_relative_path %q does not match any allow-listed root for harness %q", rel, spec.Name)
}

// Discover scans homeDir for artifacts matching spec. Symlinks at or below a
// rule root (including symlinks targeting outside the matched rule root or
// dangling links) are refused and recorded in DiscoveryResult.Refused without
// blocking discovery of valid sibling artifacts.
func Discover(homeDir string, spec HarnessSpec) (*DiscoveryResult, error) {
	if homeDir == "" {
		return nil, errors.New("home directory is required")
	}
	resolvedHome, err := pathcanon.Resolve(homeDir)
	if err != nil {
		return nil, fmt.Errorf("resolve home directory %s: %w", homeDir, err)
	}

	res := &DiscoveryResult{}
	seenRel := make(map[string]struct{})
	visitedDirs := make(map[string]struct{})

	for _, rule := range spec.Rules {
		rootRel := strings.Trim(rule.Root, "/")
		if rootRel == "" {
			continue
		}

		// Verify no component of the rule root path itself is a symlink
		// (prevents e.g. .claude/projects -> ~ from escaping the rule root).
		rootPath, ok, refuseReason, skipErr := verifyRuleRootNoSymlink(resolvedHome, rootRel)
		if skipErr != nil {
			if errors.Is(skipErr, os.ErrNotExist) {
				continue
			}
			res.Refused = append(res.Refused, RefusedPath{
				NativeRelativePath: rootRel,
				Reason:             skipErr.Error(),
			})
			continue
		}
		if !ok {
			res.Refused = append(res.Refused, RefusedPath{
				NativeRelativePath: rootRel,
				Reason:             refuseReason,
			})
			continue
		}

		within, err := pathcanon.Within(resolvedHome, rootPath)
		if err != nil || !within {
			reason := "rule root escapes home directory"
			if err != nil {
				reason = err.Error()
			}
			res.Refused = append(res.Refused, RefusedPath{
				NativeRelativePath: rootRel,
				Reason:             reason,
			})
			continue
		}
		resolvedRuleRoot, err := pathcanon.Resolve(rootPath)
		if err != nil {
			res.Refused = append(res.Refused, RefusedPath{
				NativeRelativePath: rootRel,
				Reason:             err.Error(),
			})
			continue
		}
		fi, err := os.Stat(resolvedRuleRoot)
		if err != nil || !fi.IsDir() {
			continue
		}
		walkRuleDir(resolvedRuleRoot, rootPath, rootRel, "", spec, rule, visitedDirs, seenRel, res)
	}

	sort.Slice(res.Artifacts, func(i, j int) bool {
		return res.Artifacts[i].NativeRelativePath < res.Artifacts[j].NativeRelativePath
	})
	sort.Slice(res.Refused, func(i, j int) bool {
		return res.Refused[i].NativeRelativePath < res.Refused[j].NativeRelativePath
	})
	return res, nil
}

// verifyRuleRootNoSymlink checks each component of rootRel under resolvedHome
// with os.Lstat, refusing any symlink component.
func verifyRuleRootNoSymlink(resolvedHome, rootRel string) (fullPath string, ok bool, refuseReason string, statErr error) {
	cur := resolvedHome
	for _, comp := range strings.Split(rootRel, "/") {
		if err := pathcanon.ValidName(comp); err != nil {
			return "", false, err.Error(), nil
		}
		cur = filepath.Join(cur, comp)
		lfi, err := os.Lstat(cur)
		if err != nil {
			return "", false, "", err
		}
		if lfi.Mode()&os.ModeSymlink != 0 {
			return "", false, fmt.Sprintf("rule root component %q is a symlink", comp), nil
		}
	}
	return cur, true, "", nil
}

func walkRuleDir(
	resolvedRuleRoot string,
	dirPath string,
	relPrefix string,
	relWithinRootPrefix string,
	spec HarnessSpec,
	rule ArtifactRule,
	visitedDirs map[string]struct{},
	seenRel map[string]struct{},
	res *DiscoveryResult,
) {
	resolvedDir, err := pathcanon.Resolve(dirPath)
	if err != nil {
		res.Refused = append(res.Refused, RefusedPath{
			NativeRelativePath: relPrefix,
			Reason:             err.Error(),
		})
		return
	}
	withinRoot, err := pathcanon.Within(resolvedRuleRoot, resolvedDir)
	if err != nil || !withinRoot {
		reason := "directory escapes rule root"
		if err != nil {
			reason = err.Error()
		}
		res.Refused = append(res.Refused, RefusedPath{
			NativeRelativePath: relPrefix,
			Reason:             reason,
		})
		return
	}
	if _, seen := visitedDirs[resolvedDir]; seen {
		return
	}
	visitedDirs[resolvedDir] = struct{}{}

	entries, err := os.ReadDir(dirPath)
	if err != nil {
		res.Refused = append(res.Refused, RefusedPath{
			NativeRelativePath: relPrefix,
			Reason:             fmt.Sprintf("read dir: %v", err),
		})
		return
	}

	for _, entry := range entries {
		name := entry.Name()
		childRel := relPrefix + "/" + name
		childRelWithinRoot := name
		if relWithinRootPrefix != "" {
			childRelWithinRoot = relWithinRootPrefix + "/" + name
		}
		childPath := filepath.Join(dirPath, name)

		if entry.Type()&os.ModeSymlink != 0 {
			// Refuse symlinks at both directory and leaf levels; report
			// whether the symlink escapes the rule root or is dangling.
			within, werr := pathcanon.Within(resolvedRuleRoot, childPath)
			reason := "symlink refused inside artifact tree"
			if werr != nil {
				reason = werr.Error()
			} else if !within {
				reason = "symlink escapes rule root"
			}
			res.Refused = append(res.Refused, RefusedPath{
				NativeRelativePath: childRel,
				Reason:             reason,
			})
			continue
		}
		if entry.IsDir() {
			walkRuleDir(resolvedRuleRoot, childPath, childRel, childRelWithinRoot, spec, rule, visitedDirs, seenRel, res)
			continue
		}
		if !entry.Type().IsRegular() {
			continue
		}

		if rule.Suffix != "" && !strings.HasSuffix(name, rule.Suffix) {
			continue
		}
		kind, err := ValidateNativeRelativePath(childRel, spec)
		if err != nil {
			res.Refused = append(res.Refused, RefusedPath{
				NativeRelativePath: childRel,
				Reason:             err.Error(),
			})
			continue
		}
		if _, dup := seenRel[childRel]; dup {
			continue
		}
		seenRel[childRel] = struct{}{}
		res.Artifacts = append(res.Artifacts, DiscoveredArtifact{
			NativeRelativePath: childRel,
			AbsPath:            childPath,
			RuleRootAbs:        resolvedRuleRoot,
			RelWithinRuleRoot:  childRelWithinRoot,
			Kind:               kind,
			SessionID:          extractSessionID(spec.Name, childRel),
		})
	}
}

// extractSessionID derives an optional harness session identifier from a
// validated native_relative_path.
func extractSessionID(harness, rel string) string {
	if harness != "claude" {
		return ""
	}
	// Expected Claude layouts:
	//   .claude/projects/<project>/<session>.jsonl
	//   .claude/projects/<project>/<session>/subagents/agent-<id>.jsonl
	parts := strings.Split(rel, "/")
	if len(parts) == 4 && parts[0] == ".claude" && parts[1] == "projects" && strings.HasSuffix(parts[3], ".jsonl") {
		return strings.TrimSuffix(parts[3], ".jsonl")
	}
	if len(parts) == 6 && parts[0] == ".claude" && parts[1] == "projects" && parts[4] == "subagents" {
		return parts[3]
	}
	return ""
}
