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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts/remotefetch"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/githubfetch"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	templateDriftMaxBytes     int64 = 4 << 20 // 4 MiB cap for GitHub API JSON responses
	templateDriftBudget             = 2 * time.Second
	templateDriftBranchTTL          = 60 * time.Second
	templateDriftRateLimitTTL       = 5 * time.Minute
	templateDriftCommitMax          = 1024
	templateDriftBranchMax          = 512
)

var templateDriftHosts = []string{"api.github.com"}

// templateDriftKey identifies a folder tree SHA at a specific git ref.
type templateDriftKey struct {
	owner string // lowercased
	repo  string // lowercased
	ref   string // 40-char commit SHA or branch name
	path  string // "" for repo root, else slash-separated relative folder path
}

type templateDriftCommitEntry struct {
	key     templateDriftKey
	treeSHA string
}

type templateDriftBranchEntry struct {
	treeSHA   string
	expiresAt time.Time
}

// templateDriftChecker caches GitHub tree-SHA lookups and per-repo rate-limit
// backoffs for the Hub-side template drift check at agent creation. Its zero
// value is ready for use.
type templateDriftChecker struct {
	mu sync.Mutex

	// Immutable commit tree-SHA LRU cache (no TTL).
	commitOrder *list.List
	commitItems map[templateDriftKey]*list.Element

	// Branch tree-SHA TTL cache (60s TTL).
	branchItems map[templateDriftKey]templateDriftBranchEntry

	// Per-repo 403/429 negative cache (5m TTL), keyed by lowercased "owner/repo".
	rateLimitUntil map[string]time.Time

	// Optional test seams; nil/zero select production defaults.
	fetcher templateSourceFetcher
	now     func() time.Time
	budget  time.Duration
}

func (c *templateDriftChecker) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *templateDriftChecker) effectiveBudget() time.Duration {
	if c.budget > 0 {
		return c.budget
	}
	return templateDriftBudget
}

func (c *templateDriftChecker) getFetcher(logger *slog.Logger) templateSourceFetcher {
	c.mu.Lock()
	f := c.fetcher
	c.mu.Unlock()
	if f != nil {
		return f
	}
	return newTemplateDriftFetcher(logger)
}

func newTemplateDriftFetcher(logger *slog.Logger) *remotefetch.Fetcher {
	return remotefetch.New(remotefetch.Config{
		MaxBytes:     templateDriftMaxBytes,
		Timeout:      templateDriftBudget,
		MaxRedirects: 2,
		AllowedHosts: templateDriftHosts,
		Logger:       logger,
	})
}

func (c *templateDriftChecker) isRateLimited(repoKey string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	until, ok := c.rateLimitUntil[repoKey]
	if !ok {
		return false
	}
	if !now.Before(until) {
		delete(c.rateLimitUntil, repoKey)
		return false
	}
	return true
}

func (c *templateDriftChecker) recordRateLimit(repoKey string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rateLimitUntil == nil {
		c.rateLimitUntil = make(map[string]time.Time)
	}
	for k, until := range c.rateLimitUntil {
		if !now.Before(until) {
			delete(c.rateLimitUntil, k)
		}
	}
	c.rateLimitUntil[repoKey] = now.Add(templateDriftRateLimitTTL)
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

func (c *templateDriftChecker) getBranchTree(key templateDriftKey, now time.Time) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.branchItems[key]
	if !ok {
		return "", false
	}
	if !now.Before(entry.expiresAt) {
		delete(c.branchItems, key)
		return "", false
	}
	return entry.treeSHA, true
}

func (c *templateDriftChecker) putBranchTree(key templateDriftKey, treeSHA string, now time.Time) {
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
	c.branchItems[key] = templateDriftBranchEntry{
		treeSHA:   treeSHA,
		expiresAt: now.Add(templateDriftBranchTTL),
	}
}

// templateSourceFetcher downloads one body, without or with an Authorization
// header. Retained for template_drift.go until Phase 6 migrates drift onto
// pkg/githubfetch.
type templateSourceFetcher interface {
	FetchBytes(ctx context.Context, rawURL string) (*remotefetch.Result, error)
	FetchBytesAuthorized(ctx context.Context, rawURL, authorization string) (*remotefetch.Result, error)
}

// templateGitHubSource is the parsed GitHub folder source used by
// template_drift.go until Phase 6 migrates drift onto pkg/githubfetch.
type templateGitHubSource struct {
	Owner  string
	Repo   string
	Branch string
	Path   string
}

type templateDriftTarget struct {
	src          *templateGitHubSource
	sourceCommit string
}

// resolveTemplateDriftTarget returns the validated GitHub source and commit
// when all preconditions for a drift check hold, or nil if the check should be
// skipped.
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
	branch := parsed.Ref
	if branch == "" {
		branch = "main"
	}
	src := &templateGitHubSource{
		Owner:  parsed.Owner,
		Repo:   parsed.Repo,
		Branch: branch,
		Path:   parsed.Path,
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
	if !strings.EqualFold(projOwner, src.Owner) || !strings.EqualFold(projRepo, src.Repo) {
		return nil
	}
	return &templateDriftTarget{src: src, sourceCommit: commit}
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
	repoKey := ownerLower + "/" + repoLower

	now := s.templateDrift.clock()
	if s.templateDrift.isRateLimited(repoKey, now) {
		s.agentLifecycleLog.Debug("template drift check skipped (rate-limit backoff)",
			"template_id", tmpl.ID, "project_id", project.ID, "agent_id", agentID,
			"owner", src.Owner, "repo", src.Repo)
		return ""
	}

	commitKey := templateDriftKey{owner: ownerLower, repo: repoLower, ref: target.sourceCommit, path: src.Path}
	branchKey := templateDriftKey{owner: ownerLower, repo: repoLower, ref: src.Branch, path: src.Path}

	commitTree, hasCommit := s.templateDrift.getCommitTree(commitKey)
	branchTree, hasBranch := s.templateDrift.getBranchTree(branchKey, now)

	if !hasCommit || !hasBranch {
		fetcher := s.templateDrift.getFetcher(s.resourceLog)
		cred := s.importCredential(ctx, store.TemplateScopeProject, project.ID, tmpl.SourceURL)

		if !hasCommit {
			var err error
			commitTree, err = fetchGitHubFolderTreeSHA(ctx, fetcher, cred, src, target.sourceCommit)
			if err != nil {
				s.handleTemplateDriftError(project, tmpl, target, agentID, repoKey, err)
				return ""
			}
			s.templateDrift.putCommitTree(commitKey, commitTree)
		}

		if !hasBranch {
			var err error
			branchTree, err = fetchGitHubFolderTreeSHA(ctx, fetcher, cred, src, src.Branch)
			if err != nil {
				s.handleTemplateDriftError(project, tmpl, target, agentID, repoKey, err)
				return ""
			}
			s.templateDrift.putBranchTree(branchKey, branchTree, s.templateDrift.clock())
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
	if src.Path != "" {
		sourceLoc = fmt.Sprintf("github.com/%s/%s/%s", src.Owner, src.Repo, src.Path)
	}
	shortSHA := target.sourceCommit[:7]

	s.agentLifecycleLog.Warn("template drift detected at agent create",
		"template_id", tmpl.ID,
		"template", tmplName,
		"project_id", project.ID,
		"agent_id", agentID,
		"source_commit", target.sourceCommit,
		"branch", src.Branch,
		"owner", src.Owner,
		"repo", src.Repo,
		"path", src.Path,
	)

	return fmt.Sprintf(
		"template %q may be out of date: it was imported from %s at %s, and that folder has changed on %q since. Run \"scion templates update %s\" (or Refresh from Source) to update.",
		tmplName, sourceLoc, shortSHA, src.Branch, tmplName,
	)
}

func (s *Server) handleTemplateDriftError(project *store.Project, tmpl *store.Template, target *templateDriftTarget, agentID, repoKey string, err error) {
	if isTemplateDriftRateLimited(err) {
		s.templateDrift.recordRateLimit(repoKey, s.templateDrift.clock())
	}
	s.agentLifecycleLog.Debug("template drift check failed (fail-open)",
		"template_id", tmpl.ID,
		"project_id", project.ID,
		"agent_id", agentID,
		"owner", target.src.Owner,
		"repo", target.src.Repo,
		"branch", target.src.Branch,
		"path", target.src.Path,
		"error", err,
	)
}

func isTemplateDriftRateLimited(err error) bool {
	var fe *remotefetch.Error
	if !errors.As(err, &fe) || fe.Reason != remotefetch.ReasonStatus {
		return false
	}
	return fe.Detail == "status 403" || fe.Detail == "status 429"
}

// fetchGitHubFolderTreeSHA fetches the git tree SHA of src.Path at ref using
// the GitHub REST API. For a repo-root template (src.Path == ""), it queries
// /commits/{ref} and returns .commit.tree.sha; for a subfolder, it queries
// /contents/{parent}?ref={ref} and returns the .sha of the directory entry
// matching the folder's basename.
func fetchGitHubFolderTreeSHA(ctx context.Context, fetcher templateSourceFetcher, cred string, src *templateGitHubSource, ref string) (string, error) {
	apiURL := templateDriftAPIURL(src, ref)
	var (
		res *remotefetch.Result
		err error
	)
	if cred != "" {
		res, err = fetcher.FetchBytesAuthorized(ctx, apiURL, "Bearer "+cred)
	} else {
		res, err = fetcher.FetchBytes(ctx, apiURL)
	}
	if err != nil {
		return "", err
	}

	if src.Path == "" {
		var commitResp struct {
			Commit struct {
				Tree struct {
					SHA string `json:"sha"`
				} `json:"tree"`
			} `json:"commit"`
		}
		if err := json.Unmarshal(res.Body, &commitResp); err != nil {
			return "", fmt.Errorf("parse commit response: %w", err)
		}
		if commitResp.Commit.Tree.SHA == "" {
			return "", errors.New("commit response missing tree sha")
		}
		return commitResp.Commit.Tree.SHA, nil
	}

	base := path.Base(src.Path)
	var entries []struct {
		Name string `json:"name"`
		Type string `json:"type"`
		SHA  string `json:"sha"`
	}
	if err := json.Unmarshal(res.Body, &entries); err != nil {
		return "", fmt.Errorf("parse contents response: %w", err)
	}
	for _, e := range entries {
		if e.Name == base && e.Type == "dir" && e.SHA != "" {
			return e.SHA, nil
		}
	}
	return "", fmt.Errorf("folder %q not found in directory listing", base)
}

func templateDriftAPIURL(src *templateGitHubSource, ref string) string {
	if src.Path == "" {
		return "https://api.github.com/repos/" + src.Owner + "/" + src.Repo + "/commits/" + url.PathEscape(ref)
	}
	parent := path.Dir(src.Path)
	contentsPath := ""
	if parent != "." && parent != "" {
		segs := strings.Split(parent, "/")
		escaped := make([]string, len(segs))
		for i, seg := range segs {
			escaped[i] = url.PathEscape(seg)
		}
		contentsPath = "/" + strings.Join(escaped, "/")
	}
	return "https://api.github.com/repos/" + src.Owner + "/" + src.Repo + "/contents" + contentsPath + "?ref=" + url.QueryEscape(ref)
}
