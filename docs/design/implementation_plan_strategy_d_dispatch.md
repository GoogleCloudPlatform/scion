# GitHub Pull Request Comment Webhook Integration (Strategy D)

This design document outlines the technical plan to implement Strategy D (Hybrid Dispatch with Automated Fallback) for GitHub pull request comment `@scion` mentions. This allows interactive multi-turn developer discussion with active agents on a Pull Request, while seamlessly spawning a new agent if none is currently active.

## Problem Description & Background

Currently, Scion Hub can receive and decode comments on GitHub Pull Requests and scan them for mentions of `@scion`. However, it does not yet dispatch these prompts to agents. 

We need a robust, event-driven dispatch mechanism that:
1. **Routes to Active Agents**: If an agent is already active on the PR branch, deliver the new comment directly to that agent as an inbound message. This maintains conversational state and workspace modifications.
2. **Spawns Fallback Agents**: If no agent is active, fetch the PR branch name via the GitHub API and dynamically spawn a new agent on that branch, seeding its task with the user's prompt.

---

## User Review Required

> [!IMPORTANT]
> **Authentication Scopes and GitHub App Installation:**
> - To query PR branch references from the GitHub API, the Scion Hub requires an installation-authenticated GitHub client. We will leverage Scion's existing GitHub App integration configuration.
> - Ensure that the GitHub App registration possesses the **Pull Requests** read scope.

---

## Open Questions

> [!NOTE]
> **Should agents post replies back to the GitHub PR?**
> Yes. Once an agent processes a message, its outbound replies (emitted via the message broker) can eventually be listened to by a webhook-feedback publisher to comment back on the PR. This is part of the long-term agent harness capability, but the immediate goal of this task is to ensure the **inbound routing** is fully implemented and verified.

---

## Proposed Changes

We will build the routing and dispatch logic directly in `pkg/hub/handlers_github_app_webhook.go`.

### 1. Database/Agent Labeling
To easily identify which agent belongs to which PR, we will establish a label convention:
- **`github-pr`**: Holds the string representation of the PR number (e.g., `"101"`).
- **`github-repo`**: Holds the repository full name (e.g., `"acme/widgets"`).

When looking for an active agent, we will query the store for running agents matching these labels.

### 2. Fetching PR Branch Reference
When spawning a fallback agent, we need the exact git branch name. Since the comment webhook payload only contains the PR number and repo name, we must fetch the head branch name from GitHub's PR API:
- Endpoint: `GET /repos/{owner}/{repo}/pulls/{number}`
- Key field: `head.ref` (holds the source branch name).

We will add a helper method to perform this check using the Hub's authenticated GitHub client or transport.

### 3. Dispatch Logic (The Hybrid Router)

We will update the `processComment` function in `pkg/hub/handlers_github_app_webhook.go` as follows:

```go
func (s *Server) processComment(ctx context.Context, eventType, repoFullName string, prNumber, commentID int64, body string, senderLogin string) {
	if !strings.Contains(strings.ToLower(body), "@scion") {
		return
	}

	// 1. Resolve Scion Projects associated with the repository
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
		// 2. Look for an active (running) agent labeled with this PR
		activeAgent, err := s.findActiveAgentForPR(ctx, p.ID, prNumber)
		if err != nil {
			slog.Error("Failed to query active agents for PR", "project", p.ID, "pr", prNumber, "error", err)
			continue
		}

		if activeAgent != nil {
			// --- PATH A: Route to existing active agent ---
			slog.Info("Routing GitHub mention to active agent", "agent_id", activeAgent.ID, "pr", prNumber)
			
			msg := &messages.StructuredMessage{
				Sender:      "user:github-" + senderLogin,
				Recipient:   "agent:" + activeAgent.Slug,
				RecipientID: activeAgent.ID,
				Msg:         body,
				Type:        messages.TypeInstruction,
				CreatedAt:   time.Now(),
			}
			
			// Publish to the message broker proxy
			if err := s.messageBrokerProxy.PublishMessage(ctx, p.ID, msg); err != nil {
				slog.Error("Failed to publish PR comment to agent", "agent_id", activeAgent.ID, "error", err)
			}
		} else {
			// --- PATH B: Spawn a new agent fallback ---
			slog.Info("No active agent found. Spawning dynamic fallback agent", "project", p.ID, "pr", prNumber)
			
			// A: Fetch the head branch name from GitHub
			branch, err := s.fetchPRHeadBranch(ctx, repoFullName, prNumber)
			if err != nil {
				slog.Error("Failed to fetch branch ref from GitHub", "repo", repoFullName, "pr", prNumber, "error", err)
				continue
			}

			// B: Construct a new agent creation request
			req := CreateAgentRequest{
				Name:      fmt.Sprintf("pr-%d-agent-%d", prNumber, time.Now().Unix()),
				ProjectID: p.ID,
				Branch:    branch,
				Task:      body,
				Labels: map[string]string{
					"github-pr":   strconv.FormatInt(prNumber, 10),
					"github-repo": repoFullName,
				},
			}

			// C: Provision and start the agent
			// We will invoke s.createAgentInProject internally
			go func(proj store.Project, r CreateAgentRequest) {
				// Run in background with fresh context to avoid blocking the webhook response
				bgCtx := context.Background()
				slog.Info("Starting dynamic agent dispatch", "name", r.Name, "branch", r.Branch)
				// Internal invocation of agent creation
			}(p, req)
		}
	}
}
```

---

## Verification Plan

### Automated Tests
We will implement and extend our table-driven test suites in `pkg/hub/handlers_github_app_webhook_test.go`:
1. **Active Agent Routing Test**: Mock an existing running agent in the store with `github-pr: "101"` label and verify that an incoming comment is successfully published to `messageBrokerProxy`.
2. **Dynamic Fallback Spawning Test**: Mock a scenario with no active agents, stub the GitHub API branch-fetching client, and verify that `createAgentInProject` is initiated with the correct branch, labels, and prompt.

### Verification Commands
```bash
# Run Hub unit tests
go test -v ./pkg/hub -run "TestHandleGitHubWebhook"

# Perform Go CI checks (format, lint, vet)
make ci
```
