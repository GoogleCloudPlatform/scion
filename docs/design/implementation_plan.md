# GitHub Pull Request Comment Webhook Integration

Implement webhook receivers on the Scion Hub server to support decoding GitHub pull request comments and detecting mentions of `@scion` (case insensitive). This enables the Scion server to listen to developer discussion and trigger agent actions or logging.

## User Review Required

> [!IMPORTANT]
> **Webhook Scopes on GitHub App Registration:**
> To receive these comments, the GitHub App must be configured with the following Permissions & Events:
> - **Repository Permissions**:
>   - `Metadata` (Read-only)
>   - `Pull requests` (Read & write or Read-only)
>   - `Issues` (Read & write or Read-only)
> - **Subscribe to Events**:
>   - `Issue comment`
>   - `Pull request review`
>   - `Pull request review comment`

## Proposed Changes

We will modify the GitHub webhook handler in the Scion Hub server to decode pull request comments and search for case-insensitive `@scion` mentions.

---

### [Hub Webhook Component]

#### [MODIFY] [handlers_github_app_webhook.go](file:///Users/jkrohn/Documents/code/scion/pkg/hub/handlers_github_app_webhook.go)

1. **Add payload structs** to decode pull request/issue comments and reviews:
   - `webhookIssueCommentEvent` for `issue_comment`
   - `webhookPullRequestReviewCommentEvent` for `pull_request_review_comment`
   - `webhookPullRequestReviewEvent` for `pull_request_review`

2. **Register event handlers** in the `switch eventType` block of `handleGitHubWebhook`:
   - `case "issue_comment": s.handleIssueCommentWebhook(w, r, body)`
   - `case "pull_request_review_comment": s.handlePullRequestReviewCommentWebhook(w, r, body)`
   - `case "pull_request_review": s.handlePullRequestReviewWebhook(w, r, body)`

3. **Implement processing and mention matching logic**:
   - Check if the comment body contains `@scion` (case-insensitive search).
   - Resolve the project associated with the repository using a new helper `findProjectsForRepository` (matching `extractOwnerRepo(project.GitRemote)` with `repository.full_name`).
   - Log mentions with pull request metadata, comment details, and associated project ID.

Below is the proposed struct design and handler logic:

```go
type webhookIssueCommentEvent struct {
	Action string `json:"action"` // created, edited, deleted
	Issue  struct {
		Number      int64  `json:"number"`
		HTMLURL     string `json:"html_url"`
		PullRequest *struct {
			URL string `json:"url"`
		} `json:"pull_request"` // Present only if this is a PR comment
	} `json:"issue"`
	Comment struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
	} `json:"comment"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

type webhookPullRequestReviewCommentEvent struct {
	Action      string `json:"action"` // created, edited, deleted
	PullRequest struct {
		Number  int64  `json:"number"`
		HTMLURL string `json:"html_url"`
	} `json:"pull_request"`
	Comment struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
	} `json:"comment"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

type webhookPullRequestReviewEvent struct {
	Action      string `json:"action"` // submitted, edited, dismissed
	PullRequest struct {
		Number  int64  `json:"number"`
		HTMLURL string `json:"html_url"`
	} `json:"pull_request"`
	Review struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
	} `json:"review"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}
```

```go
// handleIssueCommentWebhook handles "issue_comment" events.
func (s *Server) handleIssueCommentWebhook(w http.ResponseWriter, r *http.Request, body []byte) {
	var event webhookIssueCommentEvent
	if err := json.Unmarshal(body, &event); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid webhook payload", nil)
		return
	}

	// We only process Pull Request comments (which have the PullRequest field) and creations
	if event.Issue.PullRequest == nil || event.Action != "created" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": "not a new PR comment"})
		return
	}

	s.processComment(r.Context(), "issue_comment", event.Repository.FullName, event.Issue.Number, event.Comment.ID, event.Comment.Body)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handlePullRequestReviewCommentWebhook handles "pull_request_review_comment" events.
func (s *Server) handlePullRequestReviewCommentWebhook(w http.ResponseWriter, r *http.Request, body []byte) {
	var event webhookPullRequestReviewCommentEvent
	if err := json.Unmarshal(body, &event); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid webhook payload", nil)
		return
	}

	if event.Action != "created" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": "non-created PR review comment"})
		return
	}

	s.processComment(r.Context(), "pull_request_review_comment", event.Repository.FullName, event.PullRequest.Number, event.Comment.ID, event.Comment.Body)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handlePullRequestReviewWebhook handles "pull_request_review" events.
func (s *Server) handlePullRequestReviewWebhook(w http.ResponseWriter, r *http.Request, body []byte) {
	var event webhookPullRequestReviewEvent
	if err := json.Unmarshal(body, &event); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid webhook payload", nil)
		return
	}

	if event.Action != "submitted" || event.Review.Body == "" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": "review not submitted or empty body"})
		return
	}

	s.processComment(r.Context(), "pull_request_review", event.Repository.FullName, event.PullRequest.Number, event.Review.ID, event.Review.Body)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// processComment checks for mentions of "@scion" and resolves corresponding projects.
func (s *Server) processComment(ctx context.Context, eventType, repoFullName string, prNumber, commentID int64, body string) {
	if !strings.Contains(strings.ToLower(body), "@scion") {
		slog.Debug("No @scion mention in comment", "repo", repoFullName, "pr", prNumber, "comment_id", commentID)
		return
	}

	slog.Info("Detected @scion mention in comment!",
		"event_type", eventType,
		"repo", repoFullName,
		"pr", prNumber,
		"comment_id", commentID,
	)

	// Resolve project associated with repo
	projects, err := s.findProjectsForRepository(ctx, repoFullName)
	if err != nil {
		slog.Error("Failed to find projects for repository", "repo", repoFullName, "error", err)
		return
	}

	if len(projects) == 0 {
		slog.Warn("No project matched with the repository", "repo", repoFullName)
		return
	}

	for _, p := range projects {
		slog.Info("Matched comment mention to Scion Project",
			"project_id", p.ID,
			"project_name", p.Name,
			"git_remote", p.GitRemote,
		)
		// Future: Trigger automated agent dispatch, post responses, or spawn sub-agents
	}
}

// findProjectsForRepository looks up projects in the store matching the given repository.
func (s *Server) findProjectsForRepository(ctx context.Context, repoFullName string) ([]store.Project, error) {
	projects, err := s.store.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 10000})
	if err != nil {
		return nil, err
	}

	var matched []store.Project
	repoLower := strings.ToLower(repoFullName)
	for _, project := range projects.Items {
		if project.GitRemote == "" {
			continue
		}
		ownerRepo := extractOwnerRepo(project.GitRemote)
		if strings.ToLower(ownerRepo) == repoLower {
			matched = append(matched, project)
		}
	}
	return matched, nil
}
```

---

### [Testing and Verification]

#### [MODIFY] [handlers_github_app_webhook_test.go](file:///Users/jkrohn/Documents/code/scion/pkg/hub/handlers_github_app_webhook_test.go)

Add comprehensive unit tests in `handlers_github_app_webhook_test.go` verifying:
1. **Successful detection of "@scion" mentions** in `issue_comment`, `pull_request_review_comment`, and `pull_request_review` events (e.g. "@SCION please help", "Hey @Scion, check this").
2. **Proper mapping** from matching repository full name to the corresponding database `Project`.
3. **No-op / exclusion cases**:
   - Comments without a mention of `@scion`.
   - Non-PR issue comments (e.g. regular issues) if appropriate.
   - Comments with other actions (e.g. `edited` or `deleted`).

## Verification Plan

### Automated Tests
Run unit tests to verify correctness:
```bash
go test -v ./pkg/hub -run "TestHandleGitHubWebhook_CommentMention"
```
Verify the entire package tests pass:
```bash
go test -v ./pkg/hub
```
Run `make ci` to verify formatting, vet, and general code safety.
