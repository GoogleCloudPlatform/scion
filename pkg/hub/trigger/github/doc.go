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

// Package github normalises GitHub App webhook deliveries into provider-neutral
// trigger events and compiles server-side GitHub trigger rule presets (Triggers
// Stage 3, ptone/scion#836, ptone/scion#8088, ptone/scion#8093).
//
// It is a strict leaf package using only the Go standard library so it can be
// unit-tested and reused without importing package hub.
//
// # Supported Event Families
//
// Normalize handles the five Stage 3 content event families (§5.1):
//   - pull_request (opened, synchronize, reopened, closed, ready_for_review,
//     converted_to_draft, edited, labeled, unlabeled, assigned, unassigned,
//     review_requested, review_request_removed, plus any unlisted action with
//     common and pull-request object attributes)
//   - issues (opened, edited, closed, reopened, labeled, unlabeled, assigned,
//     unassigned, plus any unlisted action)
//   - issue_comment (created, edited, deleted, plus any unlisted action)
//   - pull_request_review (submitted, edited, dismissed, plus any unlisted action)
//   - pull_request_review_comment (created, edited, deleted, plus any unlisted action)
//
// Event families outside this table (including ping, installation,
// installation_repositories, check_run, and push prior to G2b) return
// Result{Handled: false} with a nil error.
//
// # Command Grammar and Bot Senders
//
// App-mention slash commands (@<slug> /<command> [args]) are parsed out of prose
// lines on the five creation actions only (§5.6, §16 item 3):
//   - issue_comment.created
//   - pull_request_review_comment.created
//   - pull_request_review.submitted
//   - issues.opened
//   - pull_request.opened
//
// Edits, deletions, and all other actions skip mention and command parsing.
// Mentions inside fenced code blocks, inline backtick code spans, '>'
// blockquote lines, and '<!-- ... -->' HTML comments are ignored. Commands ARE
// parsed for non-self Bot senders on those five creation
// actions (confirmed with maintainer, 2026-11-16 20:43Z, conv 8dc0db63) so rules
// and presets opting into allow_bots (§5.7, §9.2) can match
// event.attributes.command; the App's own bot events (<slug>[bot]) are dropped
// at webhook ingress before row creation (see Result.IsOwnApp) and third-party
// bots remain gated by sender_type != 'Bot' in rules by default. When Options.AppSlug
// is empty, command parsing on creation actions is skipped and
// attributes["mention_detection"] = "unavailable" is set (§10.2).
//
// # Replay Freshness and Deferred PR Comment Enrichment
//
// When Options.ReceivedAt is non-zero and more than StaleThreshold (24h) after
// the object's OccurredAt timestamp, Normalize sets attributes["stale"] = "true"
// so the dispatcher can refuse agent-creating or GitHub-commenting actions on
// replayed old deliveries (§4.4).
//
// issue_comment deliveries on pull requests carry issue.pull_request (setting
// is_pull_request = "true" and pull_request_url) without head/base branch facts.
// Drainer-side enrichment (§5.5, wired in G2 after G1 lands) later populates:
// base_ref, base_sha, head_ref, head_sha, head_repo, is_fork, draft, merged,
// state, enriched ("true" or "failed"), lists["labels"],
// lists["requested_reviewers"], and lists["enriched"]. Normalize leaves those
// PR-only keys unset on issue_comment events.
package github
