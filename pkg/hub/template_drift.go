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

package hub

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/githubfetch"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	templateDriftMaxBytes  int64 = 4 << 20 // 4 MiB cap for GitHub API JSON responses
	templateDriftBudget          = 2 * time.Second
	templateDriftBranchTTL       = 60 * time.Second
	templateDriftCommitMax       = 1024
	templateDriftBranchMax       = 512
)

// templateDriftKey identifies a cached drift lookup scoped to a GitHub
// principal (Credential.EffectiveScope()) so cached metadata never crosses
// principals.
type templateDriftKey struct {
	owner     string // lowercased
	repo      string // lowercased
	ref       string // 40-char commit SHA, ref name, or "" for repo default branch
	path      string // "" for repo root, else slash-separated relative folder path
	ambiguous bool   // true for multi-segment /tree/<rest> branch-cache keys
	scope     string // githubfetch.Credential.EffectiveScope()
}

type templateDriftCommitEntry struct {
	key     templateDriftKey
	treeSHA string
}

type templateDriftBranchEntry struct {
	resolvedRef  string
	resolvedPath string
	commitSHA    string
	treeSHA      string
	expiresAt    time.Time
}

// templateDriftCredScopeKey memoizes the actual Credential.EffectiveScope()
// returned by Server.githubCredential per (projectID, owner, repo) for the
// branch TTL. It is strictly per-project so one project's authorization result
// is never reused for another project.
type templateDriftCredScopeKey struct {
	projectID string
	owner     string // lowercased
	repo      string // lowercased
}

type templateDriftCredScopeEntry struct {
	scope     string
	expiresAt time.Time
}

// templateDriftChecker caches GitHub ref and folder tree-SHA lookups for the
// Hub-side template drift check at agent creation. Its zero value is ready for
// use. Rate-limit cooldowns use the Hub's shared githubfetch.Cooldown
// (s.githubCooldown()).
type templateDriftChecker struct {
	mu sync.Mutex

	// Immutable commit folder tree-SHA LRU cache (no TTL).
	commitOrder *list.List
	commitItems map[templateDriftKey]*list.Element

	// Ref resolution + folder tree-SHA TTL cache (60s TTL).
	branchItems map[templateDriftKey]templateDriftBranchEntry

	// Per-project memo of actual resolved Credential.EffectiveScope() (60s TTL).
	credScopes map[templateDriftCredScopeKey]templateDriftCredScopeEntry

	// Optional test seams; nil/zero select production defaults.
	client   *githubfetch.Client
	endpoint *githubfetch.Endpoint
	now      func() time.Time
	budget   time.Duration
}

func (c *templateDriftChecker) clock() time.Time {
	c.mu.Lock()
	nowFn := c.now
	c.mu.Unlock()
	if nowFn != nil {
		return nowFn()
	}
	return time.Now()
}

func (c *templateDriftChecker) effectiveBudget() time.Duration {
	c.mu.Lock()
	b := c.budget
	c.mu.Unlock()
	if b > 0 {
		return b
	}
	return templateDriftBudget
}

func (c *templateDriftChecker) getCommitTree(key templateDriftKey) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.commitItems == nil {
		return "", false
	}
	elem, ok := c.commitItems[key]
	if !ok {
		return "", false
	}
	c.commitOrder.MoveToFront(elem)
	return elem.Value.(*templateDriftCommitEntry).treeSHA, true
}

func (c *templateDriftChecker) putCommitTree(key templateDriftKey, treeSHA string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.commitItems == nil {
		c.commitOrder = list.New()
		c.commitItems = make(map[templateDriftKey]*list.Element)
	}
	if elem, ok := c.commitItems[key]; ok {
		elem.Value.(*templateDriftCommitEntry).treeSHA = treeSHA
		c.commitOrder.MoveToFront(elem)
		return
	}
	elem := c.commitOrder.PushFront(&templateDriftCommitEntry{key: key, treeSHA: treeSHA})
	c.commitItems[key] = elem
	for c.commitOrder.Len() > templateDriftCommitMax {
		back := c.commitOrder.Back()
		if back == nil {
			break
		}
		evicted := back.Value.(*templateDriftCommitEntry)
		delete(c.commitItems, evicted.key)
		c.commitOrder.Remove(back)
	}
}

func (c *templateDriftChecker) getBranchTree(key templateDriftKey, now time.Time) (templateDriftBranchEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.branchItems[key]
	if !ok {
		return templateDriftBranchEntry{}, false
	}
	if !now.Before(entry.expiresAt) {
		delete(c.branchItems, key)
		return templateDriftBranchEntry{}, false
	}
	return entry, true
}

func (c *templateDriftChecker) putBranchTree(key templateDriftKey, entry templateDriftBranchEntry, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.branchItems == nil {
		c.branchItems = make(map[templateDriftKey]templateDriftBranchEntry)
	}
	if len(c.branchItems) >= templateDriftBranchMax {
		for k, v := range c.branchItems {
			if !now.Before(v.expiresAt) {
				delete(c.branchItems, k)
			}
		}
		if len(c.branchItems) >= templateDriftBranchMax {
			var oldestKey templateDriftKey
			var oldestTime time.Time
			first := true
			for k, v := range c.branchItems {
				if first || v.expiresAt.Before(oldestTime) {
					oldestKey = k
					oldestTime = v.expiresAt
					first = false
				}
			}
			if !first {
				delete(c.branchItems, oldestKey)
			}
		}
	}
	entry.expiresAt = now.Add(templateDriftBranchTTL)
	c.branchItems[key] = entry
}

func (c *templateDriftChecker) setBranchTreeSHA(key templateDriftKey, commitSHA, treeSHA string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.branchItems[key]; ok && entry.commitSHA == commitSHA {
		entry.treeSHA = treeSHA
		c.branchItems[key] = entry
	}
}

func (c *templateDriftChecker) getCredScope(key templateDriftCredScopeKey, now time.Time) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.credScopes[key]
	if !ok {
		return "", false
	}
	if !now.Before(entry.expiresAt) {
		delete(c.credScopes, key)
		return "", false
	}
	return entry.scope, true
}

func (c *templateDriftChecker) putCredScope(key templateDriftCredScopeKey, scope string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.credScopes == nil {
		c.credScopes = make(map[templateDriftCredScopeKey]templateDriftCredScopeEntry)
	}
	if len(c.credScopes) >= templateDriftBranchMax {
		for k, v := range c.credScopes {
			if !now.Before(v.expiresAt) {
				delete(c.credScopes, k)
			}
		}
		if len(c.credScopes) >= templateDriftBranchMax {
			var oldestKey templateDriftCredScopeKey
			var oldestTime time.Time
			first := true
			for k, v := range c.credScopes {
				if first || v.expiresAt.Before(oldestTime) {
					oldestKey = k
					oldestTime = v.expiresAt
					first = false
				}
			}
			if !first {
				delete(c.credScopes, oldestKey)
			}
		}
	}
	c.credScopes[key] = templateDriftCredScopeEntry{
		scope:     scope,
		expiresAt: now.Add(templateDriftBranchTTL),
	}
}

func (s *Server) templateDriftLimits() githubfetch.Limits {
	budget := s.templateDrift.effectiveBudget()
	return githubfetch.Limits{
		MaxDownload:    templateDriftMaxBytes,
		RequestTimeout: budget,
		Timeout:        budget,
		OnSubmodule:    githubfetch.PolicySkip,
		OnSymlink:      githubfetch.PolicySkip,
	}
}

func (s *Server) getTemplateDriftClient() *githubfetch.Client {
	s.templateDrift.mu.Lock()
	c := s.templateDrift.client
	epOverride := s.templateDrift.endpoint
	nowFn := s.templateDrift.now
	s.templateDrift.mu.Unlock()
	if c != nil {
		return c
	}
	ep := githubfetch.DefaultEndpoint()
	if epOverride != nil {
		ep = *epOverride
	}
	doer := s.newTemplateGitHubDoer(ep, s.templateDriftLimits())
	return githubfetch.New(githubfetch.Options{
		Endpoint:   ep,
		Doer:       doer,
		Cooldown:   s.githubCooldown(),
		Clock:      nowFn,
		MaxRetries: 0,
	})
}

type templateDriftTarget struct {
	src          githubfetch.Source
	sourceCommit string
}

// resolveTemplateDriftTarget returns the validated GitHub source and commit
// when all preconditions for a drift check hold, or nil if the check should be
// skipped. Source URLs pinned to a full 40-hex commit SHA are immutable and
// never drift.
func (s *Server) resolveTemplateDriftTarget(project *store.Project, tmpl *store.Template) *templateDriftTarget {
	if !s.experimentEnabled(experiments.TemplateDriftWarning) {
		return nil
	}
	if project == nil || tmpl == nil {
		return nil
	}
	commit := validateSourceCommit(tmpl.SourceCommit)
	if commit == "" {
		return nil
	}
	parsed, err := githubfetch.ParseURL(tmpl.SourceURL)
	if err != nil {
		return nil
	}
	if len(parsed.RefAmbiguous) == 0 && githubfetch.IsFullCommitSHA(parsed.Ref) {
		return nil
	}
	remote := strings.TrimSpace(project.GitRemote)
	if remote == "" && project.Labels != nil {
		remote = strings.TrimSpace(project.Labels[store.LabelCloneURL])
	}
	if remote == "" {
		return nil
	}
	projOwner, projRepo, ok := githubapp.RemoteOwnerRepo(remote, "github.com")
	if !ok {
		return nil
	}
	if !strings.EqualFold(projOwner, parsed.Owner) || !strings.EqualFold(projRepo, parsed.Repo) {
		return nil
	}
	return &templateDriftTarget{src: parsed, sourceCommit: commit}
}

// startTemplateDriftCheck launches the fail-open template drift check
// concurrently under the 2s budget when preconditions hold. The caller must
// invoke the returned cancel function (e.g. via defer) and join the channel
// with awaitTemplateDriftWarning before writing CreateAgentResponse.
func (s *Server) startTemplateDriftCheck(ctx context.Context, project *store.Project, tmpl *store.Template, agentID string) (<-chan string, context.CancelFunc) {
	target := s.resolveTemplateDriftTarget(project, tmpl)
	if target == nil {
		return nil, func() {}
	}
	driftCtx, cancel := context.WithTimeout(ctx, s.templateDrift.effectiveBudget())
	ch := make(chan string, 1)
	go func() {
		ch <- s.checkTemplateDrift(driftCtx, project, tmpl, target, agentID)
	}()
	return ch, cancel
}

func awaitTemplateDriftWarning(ch <-chan string) string {
	if ch == nil {
		return ""
	}
	return <-ch
}

func (s *Server) checkTemplateDrift(ctx context.Context, project *store.Project, tmpl *store.Template, target *templateDriftTarget, agentID string) string {
	src := target.src
	ownerLower := strings.ToLower(src.Owner)
	repoLower := strings.ToLower(src.Repo)

	credReq := ghCredRequest{
		Scope:   store.TemplateScopeProject,
		ScopeID: project.ID,
		Owner:   src.Owner,
		Repo:    src.Repo,
		Quiet:   true,
	}
	scopeKey := templateDriftCredScopeKey{
		projectID: project.ID,
		owner:     ownerLower,
		repo:      repoLower,
	}
	now := s.templateDrift.clock()

	var (
		cred        githubfetch.Credential
		hasResolved bool
	)
	scope, hasScope := s.templateDrift.getCredScope(scopeKey, now)
	if !hasScope {
		cred = s.githubCredential(ctx, credReq)
		hasResolved = true
		scope = cred.EffectiveScope()
		s.templateDrift.putCredScope(scopeKey, scope, now)
	}

	branchKey := templateDriftKey{
		owner:     ownerLower,
		repo:      repoLower,
		ref:       src.Ref,
		path:      src.Path,
		ambiguous: len(src.RefAmbiguous) > 0,
		scope:     scope,
	}

	var (
		resolvedRef  string
		resolvedPath string
		headCommit   string
		branchTree   string
		commitTree   string
		hasCommit    bool
	)

	branchEntry, hasBranch := s.templateDrift.getBranchTree(branchKey, now)
	if hasBranch {
		resolvedRef = branchEntry.resolvedRef
		resolvedPath = branchEntry.resolvedPath
		headCommit = branchEntry.commitSHA
		branchTree = branchEntry.treeSHA
		if headCommit == target.sourceCommit {
			return ""
		}
		if branchTree == "" {
			headTreeKey := templateDriftKey{
				owner: ownerLower,
				repo:  repoLower,
				ref:   headCommit,
				path:  resolvedPath,
				scope: scope,
			}
			if cachedHeadTree, ok := s.templateDrift.getCommitTree(headTreeKey); ok {
				branchTree = cachedHeadTree
				s.templateDrift.setBranchTreeSHA(branchKey, headCommit, branchTree)
			}
		}
		commitKey := templateDriftKey{
			owner: ownerLower,
			repo:  repoLower,
			ref:   target.sourceCommit,
			path:  resolvedPath,
			scope: scope,
		}
		commitTree, hasCommit = s.templateDrift.getCommitTree(commitKey)
	}

	if !hasBranch || branchTree == "" || !hasCommit {
		if _, cooling := s.githubCooldown().Active(scope); cooling {
			s.agentLifecycleLog.Debug("template drift check skipped (rate-limit cooldown)",
				"template_id", tmpl.ID, "project_id", project.ID, "agent_id", agentID,
				"owner", src.Owner, "repo", src.Repo)
			return ""
		}

		// Resolve the full credential if we came from a scope memo hit and now
		// need to make an outbound GitHub API call.
		if !hasResolved {
			cred = s.githubCredential(ctx, credReq)
			actualScope := cred.EffectiveScope()
			s.templateDrift.putCredScope(scopeKey, actualScope, s.templateDrift.clock())
			if actualScope != scope {
				scope = actualScope
				branchKey.scope = scope
				hasBranch = false
				branchTree = ""
				if _, cooling := s.githubCooldown().Active(scope); cooling {
					s.agentLifecycleLog.Debug("template drift check skipped (rate-limit cooldown)",
						"template_id", tmpl.ID, "project_id", project.ID, "agent_id", agentID,
						"owner", src.Owner, "repo", src.Repo)
					return ""
				}
			}
		}

		client := s.getTemplateDriftClient()
		lim := s.templateDriftLimits()
		resolvedSrc := githubfetch.Source{
			Owner: src.Owner,
			Repo:  src.Repo,
			Ref:   resolvedRef,
			Path:  resolvedPath,
		}

		if !hasBranch {
			resolved, rSrc, err := client.ResolveRef(ctx, src, cred)
			if err != nil {
				s.handleTemplateDriftError(project, tmpl, target, agentID, err)
				return ""
			}
			resolvedRef = resolved.Ref
			resolvedPath = rSrc.Path
			headCommit = resolved.CommitSHA
			resolvedSrc = rSrc

			if resolved.Kind == githubfetch.RefSHA || headCommit == target.sourceCommit {
				s.templateDrift.putBranchTree(branchKey, templateDriftBranchEntry{
					resolvedRef:  resolvedRef,
					resolvedPath: resolvedPath,
					commitSHA:    headCommit,
				}, s.templateDrift.clock())
				return ""
			}
		}

		if branchTree == "" {
			headTreeKey := templateDriftKey{
				owner: ownerLower,
				repo:  repoLower,
				ref:   headCommit,
				path:  resolvedPath,
				scope: scope,
			}
			var hasHeadTree bool
			branchTree, hasHeadTree = s.templateDrift.getCommitTree(headTreeKey)
			if !hasHeadTree {
				tree, err := client.ListTreeShallow(ctx, resolvedSrc, headCommit, cred, lim)
				if err != nil {
					s.handleTemplateDriftError(project, tmpl, target, agentID, err)
					return ""
				}
				if tree.TreeSHA == "" {
					return ""
				}
				branchTree = tree.TreeSHA
				s.templateDrift.putCommitTree(headTreeKey, branchTree)
			}
			if !hasBranch {
				s.templateDrift.putBranchTree(branchKey, templateDriftBranchEntry{
					resolvedRef:  resolvedRef,
					resolvedPath: resolvedPath,
					commitSHA:    headCommit,
					treeSHA:      branchTree,
				}, s.templateDrift.clock())
			} else {
				s.templateDrift.setBranchTreeSHA(branchKey, headCommit, branchTree)
			}
		}

		commitKey := templateDriftKey{
			owner: ownerLower,
			repo:  repoLower,
			ref:   target.sourceCommit,
			path:  resolvedPath,
			scope: scope,
		}
		commitTree, hasCommit = s.templateDrift.getCommitTree(commitKey)
		if !hasCommit {
			tree, err := client.ListTreeShallow(ctx, resolvedSrc, target.sourceCommit, cred, lim)
			if err != nil {
				s.handleTemplateDriftError(project, tmpl, target, agentID, err)
				return ""
			}
			if tree.TreeSHA == "" {
				return ""
			}
			commitTree = tree.TreeSHA
			s.templateDrift.putCommitTree(commitKey, commitTree)
		}
	}

	if commitTree == "" || branchTree == "" || commitTree == branchTree {
		return ""
	}

	tmplName := tmpl.Slug
	if tmplName == "" {
		tmplName = tmpl.Name
	}
	sourceLoc := fmt.Sprintf("github.com/%s/%s", src.Owner, src.Repo)
	if resolvedPath != "" {
		sourceLoc = fmt.Sprintf("github.com/%s/%s/%s", src.Owner, src.Repo, resolvedPath)
	}
	shortSHA := target.sourceCommit[:7]

	s.agentLifecycleLog.Warn("template drift detected at agent create",
		"template_id", tmpl.ID,
		"template", tmplName,
		"project_id", project.ID,
		"agent_id", agentID,
		"source_commit", target.sourceCommit,
		"ref", resolvedRef,
		"owner", src.Owner,
		"repo", src.Repo,
		"path", resolvedPath,
	)

	return fmt.Sprintf(
		"template %q may be out of date: it was imported from %s at %s, and that folder has changed on %q since. Run \"scion templates update %s\" (or Refresh from Source) to update.",
		tmplName, sourceLoc, shortSHA, resolvedRef, tmplName,
	)
}

func (s *Server) handleTemplateDriftError(project *store.Project, tmpl *store.Template, target *templateDriftTarget, agentID string, err error) {
	var kind githubfetch.Kind
	var ghe *githubfetch.Error
	if errors.As(err, &ghe) {
		kind = ghe.Kind
	}
	s.agentLifecycleLog.Debug("template drift check failed (fail-open)",
		"template_id", tmpl.ID,
		"project_id", project.ID,
		"agent_id", agentID,
		"owner", target.src.Owner,
		"repo", target.src.Repo,
		"ref", target.src.Ref,
		"path", target.src.Path,
		"kind", kind,
		"error", err,
	)
}
