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

package github

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Preset schema version and canonical preset identifiers (§9.2).
const (
	PresetVersion = 1

	PresetCommand         = "github.command"
	PresetPROpened        = "github.pr_opened"
	PresetPRUpdated       = "github.pr_updated"
	PresetIssueOpened     = "github.issue_opened"
	PresetLabelAdded      = "github.label_added"
	PresetReviewSubmitted = "github.review_submitted"
)

// Preset field types (§9.2).
const (
	FieldTypeString   = "string"
	FieldTypeEnum     = "enum"
	FieldTypeBool     = "bool"
	FieldTypeDuration = "duration"
	FieldTypeList     = "list"
)

// Supported trigger action kinds and defaults emitted by presets (§7.1, §7.2, §9.2).
const (
	ActionKindMessage       = "message"
	ActionKindDispatchAgent = "dispatch_agent"
	ActionKindGitHubComment = "github.comment"

	SubjectModeNew   = "new"
	SubjectModeSkip  = "skip"
	SubjectModeReuse = "reuse"

	DialectCEL = "cel"

	DefaultSuggestedTemplate = "default"
	GitHubTokenInstallation  = "installation"

	DefaultBaseBranchSentinel = "default"
	DefaultCoalesceWindow     = "30s"
	MaxCoalesceWindow         = 10 * time.Minute

	// MaxCompiledRuleBytes mirrors triggerrule.MaxMatchRuleBytes (Stage 1 match
	// expression byte cap; pinned equal by TestMaxCompiledRuleBytes_MatchesTriggerRule)
	// without adding an in-module dependency to this stdlib-only leaf package.
	MaxCompiledRuleBytes = 4096

	maxPresetFieldKeyBytes      = 64
	maxPresetListItems          = 32
	maxPresetLabelBytes         = 100
	maxPresetBranchBytes        = 200
	maxPresetUnknownFieldErrors = 10
)

// On-target enum values for presets spanning PRs and issues (§9.2).
const (
	OnPullRequest = "pull_request"
	OnIssue       = "issue"
	OnAny         = "any"
)

var (
	onTargetEnumValues = []string{OnPullRequest, OnIssue, OnAny}

	// authorAssociationEnumValues lists GitHub's documented author_association
	// values (§5.2).
	authorAssociationEnumValues = []string{
		"OWNER",
		"MEMBER",
		"COLLABORATOR",
		"CONTRIBUTOR",
		"FIRST_TIME_CONTRIBUTOR",
		"FIRST_TIMER",
		"MANNEQUIN",
		"NONE",
	}

	defaultAllowedAssociations = []string{"OWNER", "MEMBER", "COLLABORATOR"}

	// reviewSubmittedStateEnumValues lists the normalised (lowercased, §5.3)
	// review states valid on pull_request_review.submitted. "dismissed" is
	// intentionally excluded because dismissed reviews carry historical
	// submitted_at timestamps that trip the §4.4 stale check on agent-creating
	// actions.
	reviewSubmittedStateEnumValues = []string{
		"approved",
		"changes_requested",
		"commented",
	}

	defaultReviewSubmittedStates = []string{"changes_requested"}

	defaultActionKinds = []string{
		ActionKindDispatchAgent,
		ActionKindMessage,
		ActionKindGitHubComment,
	}
)

// Preset describes a server-side trigger rule preset schema (§9.2).
type Preset struct {
	ID          string        `json:"id"`
	Source      string        `json:"source"`
	Version     int           `json:"version"`
	Title       string        `json:"title"`
	Description string        `json:"description"`
	Fields      []PresetField `json:"fields"`
	ActionKinds []string      `json:"action_kinds"`
}

// PresetField describes one configurable field in a Preset's form schema (§9.2).
type PresetField struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Enum     []string `json:"enum,omitempty"`
	Default  any      `json:"default,omitempty"`
	Required bool     `json:"required"`
	Help     string   `json:"help"`
}

// CompilePresetRequest is the input to CompilePreset and
// POST /api/v1/trigger-presets/compile (§9.2).
type CompilePresetRequest struct {
	Preset string         `json:"preset"`
	Fields map[string]any `json:"fields,omitempty"`
}

// CompiledPreset is the compiled output of CompilePreset (§9.2 rev 1.4).
type CompiledPreset struct {
	Match           CompiledMatch    `json:"match"`
	Subject         CompiledSubject  `json:"subject"`
	Form            CompiledForm     `json:"form"`
	SuggestedAction *SuggestedAction `json:"suggested_action,omitempty"`
}

// CompiledMatch holds the event-type prefilter and ordered match rules (§9.2 rev 1.4).
type CompiledMatch struct {
	Types []string       `json:"types"`
	Rules []CompiledRule `json:"rules"`
}

// CompiledRule is one dialect-tagged rule element in CompiledMatch.Rules (§9.2 rev 1.4, §9.3).
type CompiledRule struct {
	Dialect string `json:"dialect"`
	Expr    string `json:"expr"`
}

// CompiledSubject holds the compiled subject policy (§6.2, §6.3, §9.2).
type CompiledSubject struct {
	Mode           string `json:"mode"`
	CoalesceWindow string `json:"coalesce_window,omitempty"`
	Expr           string `json:"expr,omitempty"`
}

// CompiledForm is the canonicalized form state stored as Match.Form (§9.2).
type CompiledForm struct {
	Preset  string         `json:"preset"`
	Version int            `json:"version"`
	Fields  map[string]any `json:"fields"`
}

// SuggestedAckComment configures the post-fire acknowledgement comment posted
// as the GitHub App (§7.2).
type SuggestedAckComment struct {
	Body string `json:"body"`
}

// SuggestedAction holds the Stage 2 §5.1 / Stage 3 §7.2 dispatch_agent action
// specification emitted by dispatching presets. Presets always set
// githubToken="installation" and ephemeral=false (pending ptone/scion#194) and
// never set noAuth or agentRole (§7.2, §7.2a, rev 1.7).
type SuggestedAction struct {
	Type          string               `json:"type"`
	AgentName     string               `json:"agentName,omitempty"`
	Template      string               `json:"template"`
	Task          string               `json:"task"`
	Branch        string               `json:"branch,omitempty"`
	Labels        map[string]string    `json:"labels,omitempty"`
	Ephemeral     bool                 `json:"ephemeral"`
	GitHubToken   string               `json:"githubToken"`
	ReuseMessage  string               `json:"reuseMessage,omitempty"`
	AckComment    *SuggestedAckComment `json:"ackComment,omitempty"`
	UntrustedVars []string             `json:"untrustedVars"`
}

// PresetFieldError describes a content-free validation error on a single field.
type PresetFieldError struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// PresetCompileError is returned by CompilePreset when preset selection or
// field validation fails. Messages never echo caller-supplied values.
type PresetCompileError struct {
	Message string             `json:"message"`
	Fields  []PresetFieldError `json:"fields,omitempty"`
}

func (e *PresetCompileError) Error() string {
	if e == nil {
		return ""
	}
	if len(e.Fields) == 0 {
		return e.Message
	}
	parts := make([]string, len(e.Fields))
	for i, f := range e.Fields {
		parts[i] = f.Field + ": " + f.Message
	}
	return e.Message + " (" + strings.Join(parts, "; ") + ")"
}

const presetTemplatePlaceholderNote = " Emits suggested_action with template=\"default\" as a placeholder for the caller to override."

var presetRegistry = []Preset{
	{
		ID:          PresetCommand,
		Source:      SourceKind,
		Version:     PresetVersion,
		Title:       "Slash command mention",
		Description: "Fires when an authorized commenter mentions the GitHub App with a /command on a pull request or issue." + presetTemplatePlaceholderNote,
		Fields: []PresetField{
			{
				Name:     "command",
				Type:     FieldTypeString,
				Required: true,
				Help:     "Command word without leading slash (lowercase letters, digits, '_' or '-', e.g. review).",
			},
			{
				Name:     "on",
				Type:     FieldTypeEnum,
				Enum:     onTargetEnumValues,
				Default:  OnPullRequest,
				Required: false,
				Help:     "Where the command may be invoked: pull_request, issue, or any.",
			},
			{
				Name:     "allowed_associations",
				Type:     FieldTypeList,
				Enum:     authorAssociationEnumValues,
				Default:  defaultAllowedAssociations,
				Required: false,
				Help:     "Repository author_association values allowed to trigger this command.",
			},
			{
				Name:     "allow_bots",
				Type:     FieldTypeBool,
				Default:  false,
				Required: false,
				Help:     "Allow third-party Bot senders (the App's own bot events are always ignored).",
			},
			{
				Name:     "include_review_comments",
				Type:     FieldTypeBool,
				Default:  true,
				Required: false,
				Help:     "Also match commands in pull request reviews and inline diff comments (applies to on=pull_request or any only).",
			},
			{
				Name:     "include_forks",
				Type:     FieldTypeBool,
				Default:  false,
				Required: false,
				Help:     "Allow commands on pull requests opened from forks (omits automatic head branch checkout).",
			},
			{
				Name:     "include_body",
				Type:     FieldTypeBool,
				Default:  false,
				Required: false,
				Help:     "Include the comment or opening description body in the suggested agent task.",
			},
		},
		ActionKinds: defaultActionKinds,
	},
	{
		ID:          PresetPROpened,
		Source:      SourceKind,
		Version:     PresetVersion,
		Title:       "Pull request opened or ready for review",
		Description: "Fires when a pull request is opened, reopened, or marked ready for review against matching base branches." + presetTemplatePlaceholderNote,
		Fields: []PresetField{
			{
				Name:     "base_branches",
				Type:     FieldTypeList,
				Default:  []string{DefaultBaseBranchSentinel},
				Required: false,
				Help:     "Target base branches to match ('default' matches the repository's default branch, not a branch literally named default; empty matches any base branch).",
			},
			{
				Name:     "include_drafts",
				Type:     FieldTypeBool,
				Default:  false,
				Required: false,
				Help:     "Include draft pull requests before they are marked ready for review.",
			},
			{
				Name:     "include_forks",
				Type:     FieldTypeBool,
				Default:  false,
				Required: false,
				Help:     "Allow pull requests from fork repositories (omits automatic head branch checkout).",
			},
			{
				Name:     "allowed_associations",
				Type:     FieldTypeList,
				Enum:     authorAssociationEnumValues,
				Default:  defaultAllowedAssociations,
				Required: false,
				Help:     "Repository author_association values allowed to trigger this rule.",
			},
			{
				Name:     "allow_bots",
				Type:     FieldTypeBool,
				Default:  false,
				Required: false,
				Help:     "Allow third-party Bot senders (for example dependabot or renovate).",
			},
		},
		ActionKinds: defaultActionKinds,
	},
	{
		ID:          PresetPRUpdated,
		Source:      SourceKind,
		Version:     PresetVersion,
		Title:       "Pull request updated (new commits pushed)",
		Description: "Fires on pull_request.synchronize with subject reuse and burst coalescing so rapid pushes notify or start a single agent at the latest commit." + presetTemplatePlaceholderNote,
		Fields: []PresetField{
			{
				Name:     "base_branches",
				Type:     FieldTypeList,
				Default:  []string{DefaultBaseBranchSentinel},
				Required: false,
				Help:     "Target base branches to match ('default' matches the repository's default branch, not a branch literally named default; empty matches any base branch).",
			},
			{
				Name:     "include_drafts",
				Type:     FieldTypeBool,
				Default:  false,
				Required: false,
				Help:     "Include synchronize pushes on draft pull requests.",
			},
			{
				Name:     "include_forks",
				Type:     FieldTypeBool,
				Default:  false,
				Required: false,
				Help:     "Allow pull requests from fork repositories (omits automatic head branch checkout).",
			},
			{
				Name:     "allowed_associations",
				Type:     FieldTypeList,
				Enum:     authorAssociationEnumValues,
				Default:  defaultAllowedAssociations,
				Required: false,
				Help:     "Repository author_association values allowed to trigger this rule.",
			},
			{
				Name:     "allow_bots",
				Type:     FieldTypeBool,
				Default:  false,
				Required: false,
				Help:     "Allow third-party Bot senders.",
			},
			{
				Name:     "coalesce_window",
				Type:     FieldTypeDuration,
				Default:  DefaultCoalesceWindow,
				Required: false,
				Help:     "Whole-second coalescing window across rapid pushes on the same pull request (0s disables, max 10m).",
			},
		},
		ActionKinds: defaultActionKinds,
	},
	{
		ID:          PresetIssueOpened,
		Source:      SourceKind,
		Version:     PresetVersion,
		Title:       "Issue opened",
		Description: "Fires when a new issue is opened, optionally filtered to issues carrying any of the specified labels." + presetTemplatePlaceholderNote,
		Fields: []PresetField{
			{
				Name:     "labels_any",
				Type:     FieldTypeList,
				Default:  []string{},
				Required: false,
				Help:     "Optional list of issue labels; when non-empty, at least one label must be present on the issue.",
			},
			{
				Name:     "allowed_associations",
				Type:     FieldTypeList,
				Enum:     authorAssociationEnumValues,
				Default:  defaultAllowedAssociations,
				Required: false,
				Help:     "Repository author_association values allowed to trigger this rule.",
			},
			{
				Name:     "allow_bots",
				Type:     FieldTypeBool,
				Default:  false,
				Required: false,
				Help:     "Allow third-party Bot senders.",
			},
		},
		ActionKinds: defaultActionKinds,
	},
	{
		ID:          PresetLabelAdded,
		Source:      SourceKind,
		Version:     PresetVersion,
		Title:       "Label added",
		Description: "Fires when a specific label (for example agent:review) is added to a pull request or issue." + presetTemplatePlaceholderNote,
		Fields: []PresetField{
			{
				Name:     "label",
				Type:     FieldTypeString,
				Required: true,
				Help:     "Exact GitHub label name that triggers this rule when added.",
			},
			{
				Name:     "on",
				Type:     FieldTypeEnum,
				Enum:     onTargetEnumValues,
				Default:  OnAny,
				Required: false,
				Help:     "Target kind to match: pull_request, issue, or any.",
			},
			{
				Name:     "include_forks",
				Type:     FieldTypeBool,
				Default:  false,
				Required: false,
				Help:     "Allow pull requests from fork repositories when matching pull request label events.",
			},
			{
				Name:     "allow_bots",
				Type:     FieldTypeBool,
				Default:  false,
				Required: false,
				Help:     "Allow third-party Bot senders applying the label.",
			},
		},
		ActionKinds: defaultActionKinds,
	},
	{
		ID:          PresetReviewSubmitted,
		Source:      SourceKind,
		Version:     PresetVersion,
		Title:       "Pull request review submitted",
		Description: "Fires when a pull request review is submitted in one of the selected states (for example changes_requested)." + presetTemplatePlaceholderNote,
		Fields: []PresetField{
			{
				Name:     "states",
				Type:     FieldTypeList,
				Enum:     reviewSubmittedStateEnumValues,
				Default:  defaultReviewSubmittedStates,
				Required: false,
				Help:     "Review states to match: approved, changes_requested, commented.",
			},
			{
				Name:     "allowed_associations",
				Type:     FieldTypeList,
				Enum:     authorAssociationEnumValues,
				Default:  defaultAllowedAssociations,
				Required: false,
				Help:     "Reviewer repository author_association values allowed to trigger this rule.",
			},
			{
				Name:     "include_forks",
				Type:     FieldTypeBool,
				Default:  false,
				Required: false,
				Help:     "Allow reviews on pull requests from fork repositories (omits automatic head branch checkout).",
			},
			{
				Name:     "allow_bots",
				Type:     FieldTypeBool,
				Default:  false,
				Required: false,
				Help:     "Allow third-party Bot reviewers.",
			},
		},
		ActionKinds: defaultActionKinds,
	},
}

// Presets returns all registered trigger presets matching source (or all
// presets when source is empty). Returned structs and slices are defensive copies.
func Presets(source string) []Preset {
	filter := strings.TrimSpace(source)
	out := make([]Preset, 0, len(presetRegistry))
	for i := range presetRegistry {
		if filter != "" && presetRegistry[i].Source != filter {
			continue
		}
		out = append(out, clonePreset(presetRegistry[i]))
	}
	return out
}

// LookupPreset returns a defensive copy of the preset schema with id, if registered.
func LookupPreset(id string) (Preset, bool) {
	for i := range presetRegistry {
		if presetRegistry[i].ID == id {
			return clonePreset(presetRegistry[i]), true
		}
	}
	return Preset{}, false
}

func clonePreset(p Preset) Preset {
	cp := p
	if p.Fields != nil {
		cp.Fields = make([]PresetField, len(p.Fields))
		for i, f := range p.Fields {
			cf := f
			if f.Enum != nil {
				cf.Enum = append([]string(nil), f.Enum...)
			}
			if defSlice, ok := f.Default.([]string); ok && defSlice != nil {
				cf.Default = append([]string(nil), defSlice...)
			}
			cp.Fields[i] = cf
		}
	}
	if p.ActionKinds != nil {
		cp.ActionKinds = append([]string(nil), p.ActionKinds...)
	}
	return cp
}

// CompilePreset validates req against the named preset's field schema and
// compiles it into event types, an ordered CEL rule list, subject policy,
// canonicalized Match.Form, and suggested dispatch_agent action (§7.2, §9.2).
// Validation failures return *PresetCompileError with content-free messages.
func CompilePreset(req CompilePresetRequest) (*CompiledPreset, error) {
	presetID := strings.TrimSpace(req.Preset)
	if presetID == "" {
		return nil, &PresetCompileError{
			Message: "preset validation failed",
			Fields: []PresetFieldError{{
				Field:   "preset",
				Code:    "required",
				Message: "preset identifier is required",
			}},
		}
	}
	schema, ok := LookupPreset(presetID)
	if !ok {
		return nil, &PresetCompileError{
			Message: "preset validation failed",
			Fields: []PresetFieldError{{
				Field:   "preset",
				Code:    "unknown_preset",
				Message: "unknown trigger preset",
			}},
		}
	}

	knownFields := make(map[string]PresetField, len(schema.Fields))
	for _, f := range schema.Fields {
		knownFields[f.Name] = f
	}

	var fieldErrs []PresetFieldError

	// Reject unknown fields in deterministic sorted order.
	if len(req.Fields) > 0 {
		var unknownKeys []string
		for k := range req.Fields {
			if _, exists := knownFields[k]; !exists {
				unknownKeys = append(unknownKeys, k)
			}
		}
		sort.Strings(unknownKeys)
		limit := len(unknownKeys)
		if limit > maxPresetUnknownFieldErrors {
			limit = maxPresetUnknownFieldErrors
		}
		for _, rawKey := range unknownKeys[:limit] {
			fieldErrs = append(fieldErrs, PresetFieldError{
				Field:   sanitizeFieldKeyForError(rawKey),
				Code:    "unknown_field",
				Message: "unknown field for preset",
			})
		}
		if omitted := len(unknownKeys) - limit; omitted > 0 {
			fieldErrs = append(fieldErrs, PresetFieldError{
				Field:   "fields",
				Code:    "too_many_unknown_fields",
				Message: fmt.Sprintf("%d additional unknown fields omitted", omitted),
			})
		}
	}

	reader := &presetFieldReader{
		schema: knownFields,
		raw:    req.Fields,
		errs:   &fieldErrs,
	}

	var compiled *CompiledPreset
	switch schema.ID {
	case PresetCommand:
		compiled = compileCommandPreset(reader)
	case PresetPROpened:
		compiled = compilePROpenedPreset(reader)
	case PresetPRUpdated:
		compiled = compilePRUpdatedPreset(reader)
	case PresetIssueOpened:
		compiled = compileIssueOpenedPreset(reader)
	case PresetLabelAdded:
		compiled = compileLabelAddedPreset(reader)
	case PresetReviewSubmitted:
		compiled = compileReviewSubmittedPreset(reader)
	}

	if len(fieldErrs) > 0 {
		return nil, &PresetCompileError{
			Message: "preset field validation failed",
			Fields:  fieldErrs,
		}
	}
	return compiled, nil
}

func compileCommandPreset(r *presetFieldReader) *CompiledPreset {
	cmd := r.readCommand("command")
	on := r.readEnum("on")
	assocs := r.readEnumList("allowed_associations", true)
	allowBots := r.readBool("allow_bots")
	includeReview := r.readBool("include_review_comments")
	includeForks := r.readBool("include_forks")
	includeBody := r.readBool("include_body")
	if r.hasErrors() {
		return nil
	}

	types := []string{SourceKind + "." + EventIssueComment + ".created"}
	if on == OnPullRequest || on == OnAny {
		if includeReview {
			types = append(types,
				SourceKind+"."+EventPullRequestReviewComment+".created",
				SourceKind+"."+EventPullRequestReview+".submitted",
			)
		}
	}
	if on == OnIssue || on == OnAny {
		types = append(types, SourceKind+"."+EventIssues+".opened")
	}
	if on == OnPullRequest || on == OnAny {
		types = append(types, SourceKind+"."+EventPullRequest+".opened")
	}

	var clauses []string
	clauses = append(clauses, "event.attributes.command == "+celQuoteString(cmd))
	// Fork guards intentionally omit `has(event.attributes.is_fork)` so they
	// FAIL CLOSED when `is_fork` is absent (for example on PR issue_comment
	// events before §5.5 enrichment runs or when enrichment fails). A missing
	// `is_fork` key produces a CEL evaluation error, causing the trigger engine
	// to record the fire as skipped rather than letting fork-PR comments through.
	// On `on=any`, `event.attributes.is_pull_request == 'false'` short-circuits
	// plain issue events while still failing closed on un-enriched PR events.
	switch on {
	case OnPullRequest:
		clauses = append(clauses, "event.attributes.is_pull_request == 'true'")
		if !includeForks {
			clauses = append(clauses, "event.attributes.is_fork == 'false'")
		}
	case OnIssue:
		clauses = append(clauses, "event.attributes.is_pull_request == 'false'")
	case OnAny:
		if !includeForks {
			clauses = append(clauses, "(event.attributes.is_pull_request == 'false' || event.attributes.is_fork == 'false')")
		}
	}
	clauses = append(clauses, celInListExpr("event.attributes.author_association", assocs))
	if !allowBots {
		clauses = append(clauses, "event.attributes.sender_type != 'Bot'")
	}

	namePrefix := "gh"
	switch on {
	case OnPullRequest:
		namePrefix = "pr"
	case OnIssue:
		namePrefix = "issue"
	}
	agentName := fmt.Sprintf("%s-${event.attributes.number}-%s", namePrefix, commandAgentNameSuffix(cmd))

	branch := ""
	if on == OnPullRequest && !includeForks {
		branch = "${event.attributes.head_ref}"
	}

	var task strings.Builder
	fmt.Fprintf(&task, "Handle `/%s` on ${event.attributes.repo}#${event.attributes.number} (%s) requested by @${event.attributes.sender}.\n", cmd, "${event.attributes.title}")
	task.WriteString("Command arguments: ${event.attributes.command_args?}\n")
	task.WriteString("Source URL: ${event.attributes.html_url}\n")
	if includeBody {
		task.WriteString("Event body:\n${event.attributes.body?}\n")
	}
	if on != OnIssue && includeForks {
		task.WriteString("If this is a fork pull request, inspect and check out the PR head safely with `gh pr checkout ${event.attributes.number}`.\n")
	}
	task.WriteString(agentReplyGuidanceLine)

	sa := buildSuggestedAction(suggestedActionParams{
		agentName: agentName,
		template:  DefaultSuggestedTemplate,
		task:      task.String(),
		branch:    branch,
		labels: map[string]string{
			"issue":          "${event.attributes.repo}#${event.attributes.number}",
			"github_command": "${event.attributes.command}",
		},
		reuseMessage: "New ${event.type} on ${event.subject}: ${event.attributes.action} by @${event.attributes.sender}. Command: /${event.attributes.command?} ${event.attributes.command_args?}",
		ackBody:      "Started **${result.agent_name}** for `/${event.attributes.command}` — [open agent](${result.agent_url}). Requested by @${event.attributes.sender}.",
	})

	expr := strings.Join(clauses, " && ")
	if !r.checkCompiledRuleLength("command", expr) {
		return nil
	}

	return &CompiledPreset{
		Match: CompiledMatch{
			Types: types,
			Rules: []CompiledRule{{
				Dialect: DialectCEL,
				Expr:    expr,
			}},
		},
		Subject: CompiledSubject{
			Mode: SubjectModeNew,
		},
		Form: CompiledForm{
			Preset:  PresetCommand,
			Version: PresetVersion,
			Fields: map[string]any{
				"command":                 cmd,
				"on":                      on,
				"allowed_associations":    assocs,
				"allow_bots":              allowBots,
				"include_review_comments": includeReview,
				"include_forks":           includeForks,
				"include_body":            includeBody,
			},
		},
		SuggestedAction: sa,
	}
}

func compilePROpenedPreset(r *presetFieldReader) *CompiledPreset {
	baseBranches := r.readBaseBranches("base_branches")
	includeDrafts := r.readBool("include_drafts")
	includeForks := r.readBool("include_forks")
	assocs := r.readEnumList("allowed_associations", true)
	allowBots := r.readBool("allow_bots")
	if r.hasErrors() {
		return nil
	}

	types := []string{
		SourceKind + "." + EventPullRequest + ".opened",
		SourceKind + "." + EventPullRequest + ".ready_for_review",
		SourceKind + "." + EventPullRequest + ".reopened",
	}
	expr := buildPRFilterExpr(baseBranches, includeDrafts, includeForks, assocs, allowBots)
	if !r.checkCompiledRuleLength("base_branches", expr) {
		return nil
	}

	branch := "${event.attributes.head_ref}"
	if includeForks {
		branch = ""
	}

	var task strings.Builder
	task.WriteString("Review pull request ${event.attributes.repo}#${event.attributes.number} (${event.attributes.title}) opened by @${event.attributes.sender} targeting `${event.attributes.base_ref}` at commit `${event.attributes.head_sha}`.\n")
	task.WriteString("Pull request URL: ${event.attributes.html_url}\n")
	if includeForks {
		task.WriteString("This rule admits fork pull requests (`is_fork=${event.attributes.is_fork}`); check out the PR head with `gh pr checkout ${event.attributes.number}`.\n")
	}
	task.WriteString(agentReplyGuidanceLine)

	sa := buildSuggestedAction(suggestedActionParams{
		agentName: "pr-${event.attributes.number}-opened",
		template:  DefaultSuggestedTemplate,
		task:      task.String(),
		branch:    branch,
		labels: map[string]string{
			"issue": "${event.attributes.repo}#${event.attributes.number}",
		},
		reuseMessage: "New ${event.type} on ${event.subject}: ${event.attributes.action} by @${event.attributes.sender} at ${event.attributes.head_sha}.",
		ackBody:      "Started **${result.agent_name}** for ${event.subject} — [open agent](${result.agent_url}). Triggered by @${event.attributes.sender}.",
	})

	return &CompiledPreset{
		Match: CompiledMatch{
			Types: types,
			Rules: []CompiledRule{{
				Dialect: DialectCEL,
				Expr:    expr,
			}},
		},
		Subject: CompiledSubject{
			Mode: SubjectModeNew,
		},
		Form: CompiledForm{
			Preset:  PresetPROpened,
			Version: PresetVersion,
			Fields: map[string]any{
				"base_branches":        baseBranches,
				"include_drafts":       includeDrafts,
				"include_forks":        includeForks,
				"allowed_associations": assocs,
				"allow_bots":           allowBots,
			},
		},
		SuggestedAction: sa,
	}
}

func compilePRUpdatedPreset(r *presetFieldReader) *CompiledPreset {
	baseBranches := r.readBaseBranches("base_branches")
	includeDrafts := r.readBool("include_drafts")
	includeForks := r.readBool("include_forks")
	assocs := r.readEnumList("allowed_associations", true)
	allowBots := r.readBool("allow_bots")
	windowStr, windowDur := r.readCoalesceWindow("coalesce_window")
	if r.hasErrors() {
		return nil
	}

	types := []string{
		SourceKind + "." + EventPullRequest + ".synchronize",
	}
	expr := buildPRFilterExpr(baseBranches, includeDrafts, includeForks, assocs, allowBots)
	if !r.checkCompiledRuleLength("base_branches", expr) {
		return nil
	}

	branch := "${event.attributes.head_ref}"
	if includeForks {
		branch = ""
	}

	var task strings.Builder
	task.WriteString("Inspect updated commits on pull request ${event.attributes.repo}#${event.attributes.number} (${event.attributes.title}) pushed by @${event.attributes.sender} targeting `${event.attributes.base_ref}` at head `${event.attributes.head_sha}`.\n")
	task.WriteString("Pull request URL: ${event.attributes.html_url}\n")
	if includeForks {
		task.WriteString("This rule admits fork pull requests (`is_fork=${event.attributes.is_fork}`); check out the PR head with `gh pr checkout ${event.attributes.number}`.\n")
	}
	task.WriteString(agentReplyGuidanceLine)

	sa := buildSuggestedAction(suggestedActionParams{
		agentName: "pr-${event.attributes.number}-sync",
		template:  DefaultSuggestedTemplate,
		task:      task.String(),
		branch:    branch,
		labels: map[string]string{
			"issue": "${event.attributes.repo}#${event.attributes.number}",
		},
		reuseMessage: "New ${event.type} on ${event.subject}: ${event.attributes.action} by @${event.attributes.sender} at head ${event.attributes.head_sha}.",
		ackBody:      "Started **${result.agent_name}** for ${event.subject} — [open agent](${result.agent_url}). Triggered by @${event.attributes.sender}.",
	})

	subj := CompiledSubject{
		Mode: SubjectModeReuse,
	}
	if windowDur > 0 {
		subj.CoalesceWindow = windowStr
	}

	return &CompiledPreset{
		Match: CompiledMatch{
			Types: types,
			Rules: []CompiledRule{{
				Dialect: DialectCEL,
				Expr:    expr,
			}},
		},
		Subject: subj,
		Form: CompiledForm{
			Preset:  PresetPRUpdated,
			Version: PresetVersion,
			Fields: map[string]any{
				"base_branches":        baseBranches,
				"include_drafts":       includeDrafts,
				"include_forks":        includeForks,
				"allowed_associations": assocs,
				"allow_bots":           allowBots,
				"coalesce_window":      windowStr,
			},
		},
		SuggestedAction: sa,
	}
}

func compileIssueOpenedPreset(r *presetFieldReader) *CompiledPreset {
	labelsAny := r.readLabelsList("labels_any")
	assocs := r.readEnumList("allowed_associations", true)
	allowBots := r.readBool("allow_bots")
	if r.hasErrors() {
		return nil
	}

	types := []string{
		SourceKind + "." + EventIssues + ".opened",
	}

	var clauses []string
	if len(labelsAny) > 0 {
		clauses = append(clauses, fmt.Sprintf("event.lists.labels.exists(l, %s)", celInListExpr("l", labelsAny)))
	}
	clauses = append(clauses, celInListExpr("event.attributes.author_association", assocs))
	if !allowBots {
		clauses = append(clauses, "event.attributes.sender_type != 'Bot'")
	}

	var task strings.Builder
	task.WriteString("Triage newly opened issue ${event.attributes.repo}#${event.attributes.number} (${event.attributes.title}) reported by @${event.attributes.sender}.\n")
	task.WriteString("Issue URL: ${event.attributes.html_url}\n")
	task.WriteString("Issue body:\n${event.attributes.body?}\n")
	task.WriteString(agentReplyGuidanceLine)

	sa := buildSuggestedAction(suggestedActionParams{
		agentName: "issue-${event.attributes.number}-triage",
		template:  DefaultSuggestedTemplate,
		task:      task.String(),
		labels: map[string]string{
			"issue": "${event.attributes.repo}#${event.attributes.number}",
		},
		reuseMessage: "New ${event.type} on ${event.subject}: ${event.attributes.action} by @${event.attributes.sender}.",
		ackBody:      "Started **${result.agent_name}** for ${event.subject} — [open agent](${result.agent_url}). Triggered by @${event.attributes.sender}.",
	})

	expr := strings.Join(clauses, " && ")
	if !r.checkCompiledRuleLength("labels_any", expr) {
		return nil
	}

	return &CompiledPreset{
		Match: CompiledMatch{
			Types: types,
			Rules: []CompiledRule{{
				Dialect: DialectCEL,
				Expr:    expr,
			}},
		},
		Subject: CompiledSubject{
			Mode: SubjectModeNew,
		},
		Form: CompiledForm{
			Preset:  PresetIssueOpened,
			Version: PresetVersion,
			Fields: map[string]any{
				"labels_any":           labelsAny,
				"allowed_associations": assocs,
				"allow_bots":           allowBots,
			},
		},
		SuggestedAction: sa,
	}
}

func compileLabelAddedPreset(r *presetFieldReader) *CompiledPreset {
	label := r.readLabel("label")
	on := r.readEnum("on")
	includeForks := r.readBool("include_forks")
	allowBots := r.readBool("allow_bots")
	if r.hasErrors() {
		return nil
	}

	var types []string
	var clauses []string
	clauses = append(clauses, "event.attributes.label == "+celQuoteString(label))

	switch on {
	case OnPullRequest:
		types = []string{SourceKind + "." + EventPullRequest + ".labeled"}
		clauses = append(clauses, "event.attributes.is_pull_request == 'true'")
		if !includeForks {
			clauses = append(clauses, "event.attributes.is_fork == 'false'")
		}
	case OnIssue:
		types = []string{SourceKind + "." + EventIssues + ".labeled"}
		clauses = append(clauses, "event.attributes.is_pull_request == 'false'")
	case OnAny:
		types = []string{
			SourceKind + "." + EventPullRequest + ".labeled",
			SourceKind + "." + EventIssues + ".labeled",
		}
		// Same fail-closed fork guard as github.command: plain issues
		// short-circuit on `is_pull_request == 'false'`, while PR events require
		// `is_fork == 'false'` and skip on missing `is_fork`.
		if !includeForks {
			clauses = append(clauses, "(event.attributes.is_pull_request == 'false' || event.attributes.is_fork == 'false')")
		}
	}
	if !allowBots {
		clauses = append(clauses, "event.attributes.sender_type != 'Bot'")
	}

	namePrefix := "gh"
	switch on {
	case OnPullRequest:
		namePrefix = "pr"
	case OnIssue:
		namePrefix = "issue"
	}
	branch := ""
	if on == OnPullRequest && !includeForks {
		branch = "${event.attributes.head_ref}"
	}

	var task strings.Builder
	task.WriteString("Handle label `${event.attributes.label}` added to ${event.attributes.repo}#${event.attributes.number} (${event.attributes.title}) by @${event.attributes.sender}.\n")
	task.WriteString("Source URL: ${event.attributes.html_url}\n")
	if on != OnIssue && includeForks {
		task.WriteString("If this is a fork pull request, inspect and check out the PR head safely with `gh pr checkout ${event.attributes.number}`.\n")
	}
	task.WriteString(agentReplyGuidanceLine)

	sa := buildSuggestedAction(suggestedActionParams{
		agentName: namePrefix + "-${event.attributes.number}-labeled",
		template:  DefaultSuggestedTemplate,
		task:      task.String(),
		branch:    branch,
		labels: map[string]string{
			"issue": "${event.attributes.repo}#${event.attributes.number}",
		},
		reuseMessage: "New ${event.type} on ${event.subject}: label `${event.attributes.label}` added by @${event.attributes.sender}.",
		ackBody:      "Started **${result.agent_name}** for ${event.subject} — [open agent](${result.agent_url}). Triggered by @${event.attributes.sender}.",
	})

	expr := strings.Join(clauses, " && ")
	if !r.checkCompiledRuleLength("label", expr) {
		return nil
	}

	return &CompiledPreset{
		Match: CompiledMatch{
			Types: types,
			Rules: []CompiledRule{{
				Dialect: DialectCEL,
				Expr:    expr,
			}},
		},
		Subject: CompiledSubject{
			Mode: SubjectModeNew,
		},
		Form: CompiledForm{
			Preset:  PresetLabelAdded,
			Version: PresetVersion,
			Fields: map[string]any{
				"label":         label,
				"on":            on,
				"include_forks": includeForks,
				"allow_bots":    allowBots,
			},
		},
		SuggestedAction: sa,
	}
}

func compileReviewSubmittedPreset(r *presetFieldReader) *CompiledPreset {
	states := r.readEnumList("states", true)
	assocs := r.readEnumList("allowed_associations", true)
	includeForks := r.readBool("include_forks")
	allowBots := r.readBool("allow_bots")
	if r.hasErrors() {
		return nil
	}

	types := []string{
		SourceKind + "." + EventPullRequestReview + ".submitted",
	}

	var clauses []string
	clauses = append(clauses, celInListExpr("event.attributes.review_state", states))
	if !includeForks {
		clauses = append(clauses, "event.attributes.is_fork == 'false'")
	}
	clauses = append(clauses, celInListExpr("event.attributes.author_association", assocs))
	if !allowBots {
		clauses = append(clauses, "event.attributes.sender_type != 'Bot'")
	}

	branch := "${event.attributes.head_ref}"
	if includeForks {
		branch = ""
	}

	var task strings.Builder
	task.WriteString("Handle `${event.attributes.review_state}` review submitted by @${event.attributes.sender} on pull request ${event.attributes.repo}#${event.attributes.number} (${event.attributes.title}).\n")
	task.WriteString("Review URL: ${event.attributes.html_url}\n")
	task.WriteString("Review summary:\n${event.attributes.body?}\n")
	if includeForks {
		task.WriteString("This rule admits fork pull requests (`is_fork=${event.attributes.is_fork}`); check out the PR head with `gh pr checkout ${event.attributes.number}`.\n")
	}
	task.WriteString(agentReplyGuidanceLine)

	sa := buildSuggestedAction(suggestedActionParams{
		agentName: "pr-${event.attributes.number}-review-reply",
		template:  DefaultSuggestedTemplate,
		task:      task.String(),
		branch:    branch,
		labels: map[string]string{
			"issue": "${event.attributes.repo}#${event.attributes.number}",
		},
		reuseMessage: "New ${event.type} on ${event.subject}: `${event.attributes.review_state}` review by @${event.attributes.sender}.",
		ackBody:      "Started **${result.agent_name}** for ${event.subject} — [open agent](${result.agent_url}). Triggered by @${event.attributes.sender}.",
	})

	expr := strings.Join(clauses, " && ")
	if !r.checkCompiledRuleLength("states", expr) {
		return nil
	}

	return &CompiledPreset{
		Match: CompiledMatch{
			Types: types,
			Rules: []CompiledRule{{
				Dialect: DialectCEL,
				Expr:    expr,
			}},
		},
		Subject: CompiledSubject{
			Mode: SubjectModeNew,
		},
		Form: CompiledForm{
			Preset:  PresetReviewSubmitted,
			Version: PresetVersion,
			Fields: map[string]any{
				"states":               states,
				"allowed_associations": assocs,
				"include_forks":        includeForks,
				"allow_bots":           allowBots,
			},
		},
		SuggestedAction: sa,
	}
}

const agentReplyGuidanceLine = "Use `gh` (authenticated as the GitHub App installation) to post results on the PR/issue, mention @${event.attributes.sender} when answering, and never include an `@<app> /<command>` slash-command mention in comments."

func buildPRFilterExpr(baseBranches []string, includeDrafts, includeForks bool, assocs []string, allowBots bool) string {
	var clauses []string
	if len(baseBranches) > 0 {
		clauses = append(clauses, buildBaseBranchExpr(baseBranches))
	}
	if !includeDrafts {
		clauses = append(clauses, "event.attributes.draft == 'false'")
	}
	if !includeForks {
		clauses = append(clauses, "event.attributes.is_fork == 'false'")
	}
	clauses = append(clauses, celInListExpr("event.attributes.author_association", assocs))
	if !allowBots {
		clauses = append(clauses, "event.attributes.sender_type != 'Bot'")
	}
	return strings.Join(clauses, " && ")
}

func buildBaseBranchExpr(branches []string) string {
	hasDefault := false
	var literals []string
	for _, b := range branches {
		if b == DefaultBaseBranchSentinel {
			hasDefault = true
		} else {
			literals = append(literals, b)
		}
	}
	switch {
	case hasDefault && len(literals) == 0:
		return "event.attributes.base_ref == event.attributes.default_branch"
	case !hasDefault && len(literals) == 1:
		return "event.attributes.base_ref == " + celQuoteString(literals[0])
	case !hasDefault:
		return celInListExpr("event.attributes.base_ref", literals)
	case len(literals) == 1:
		return "(event.attributes.base_ref == event.attributes.default_branch || event.attributes.base_ref == " + celQuoteString(literals[0]) + ")"
	default:
		return "(event.attributes.base_ref == event.attributes.default_branch || " + celInListExpr("event.attributes.base_ref", literals) + ")"
	}
}

type suggestedActionParams struct {
	agentName    string
	template     string
	task         string
	branch       string
	labels       map[string]string
	reuseMessage string
	ackBody      string
}

func buildSuggestedAction(p suggestedActionParams) *SuggestedAction {
	var templates []string
	templates = append(templates, p.task)
	if p.agentName != "" {
		templates = append(templates, p.agentName)
	}
	if p.branch != "" {
		templates = append(templates, p.branch)
	}
	if len(p.labels) > 0 {
		keys := make([]string, 0, len(p.labels))
		for k := range p.labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			templates = append(templates, p.labels[k])
		}
	}
	if p.reuseMessage != "" {
		templates = append(templates, p.reuseMessage)
	}
	if p.ackBody != "" {
		templates = append(templates, p.ackBody)
	}

	sa := &SuggestedAction{
		Type:          ActionKindDispatchAgent,
		AgentName:     p.agentName,
		Template:      p.template,
		Task:          p.task,
		Branch:        p.branch,
		Labels:        p.labels,
		Ephemeral:     false,
		GitHubToken:   GitHubTokenInstallation,
		ReuseMessage:  p.reuseMessage,
		UntrustedVars: collectUntrustedTemplateVars(templates...),
	}
	if p.ackBody != "" {
		sa.AckComment = &SuggestedAckComment{Body: p.ackBody}
	}
	return sa
}

var trustedTemplateVars = map[string]bool{
	"event.id":          true,
	"event.source.kind": true,
	"event.source.id":   true,
	"event.source.name": true,
	"event.received_at": true,
	"trigger.id":        true,
	"trigger.name":      true,
	"project.id":        true,
	"project.name":      true,
	"result.agent_id":   true,
	"result.agent_name": true,
	"result.agent_url":  true,
	"result.fire_id":    true,
}

func collectUntrustedTemplateVars(templates ...string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0)
	for _, raw := range templates {
		for i := 0; i < len(raw); {
			idx := strings.IndexByte(raw[i:], '$')
			if idx < 0 {
				break
			}
			i += idx
			if strings.HasPrefix(raw[i:], "$${") {
				i += 3
				continue
			}
			if !strings.HasPrefix(raw[i:], "${") {
				i++
				continue
			}
			closeRel := strings.IndexByte(raw[i+2:], '}')
			if closeRel < 0 {
				break
			}
			path := strings.TrimSuffix(raw[i+2:i+2+closeRel], "?")
			if path != "" && !trustedTemplateVars[path] && !seen[path] {
				seen[path] = true
				out = append(out, path)
			}
			i += 2 + closeRel + 1
		}
	}
	return out
}

// commandAgentNameSuffix converts a validated command (`^[a-z][a-z0-9_-]{0,31}$`)
// into a canonical agent-name slug segment (`^[a-z0-9-]{1,24}$`) so the
// rendered name always passes api.ValidateAgentName without normalization.
func commandAgentNameSuffix(cmd string) string {
	s := strings.ReplaceAll(cmd, "_", "-")
	// Collapse consecutive hyphens so canonical slug equality holds.
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	if len(s) > 24 {
		s = s[:24]
	}
	s = strings.TrimRight(s, "-")
	if s == "" {
		return "cmd"
	}
	return s
}

// celQuoteString formats s as a single-quoted CEL string literal, escaping
// every backslash, single quote, and non-printable/line-separator rune so user
// text can never break out of the CEL string literal.
func celQuoteString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 4)
	b.WriteByte('\'')
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\'':
			b.WriteString(`\'`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f || r == '\u2028' || r == '\u2029' || unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			if r <= 0xFFFF {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				fmt.Fprintf(&b, `\U%08x`, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

func celInListExpr(lhs string, items []string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = celQuoteString(item)
	}
	return lhs + " in [" + strings.Join(quoted, ", ") + "]"
}

type presetFieldReader struct {
	schema map[string]PresetField
	raw    map[string]any
	errs   *[]PresetFieldError
}

func (r *presetFieldReader) addError(field, code, msg string) {
	*r.errs = append(*r.errs, PresetFieldError{
		Field:   field,
		Code:    code,
		Message: msg,
	})
}

func (r *presetFieldReader) hasErrors() bool {
	return len(*r.errs) > 0
}

func (r *presetFieldReader) checkCompiledRuleLength(field, expr string) bool {
	if len(expr) > MaxCompiledRuleBytes {
		r.addError(field, "too_long", fmt.Sprintf("compiled match expression exceeds maximum of %d bytes", MaxCompiledRuleBytes))
		return false
	}
	return true
}

func (r *presetFieldReader) rawOrDefault(name string) (any, bool) {
	if r.raw != nil {
		if v, exists := r.raw[name]; exists && v != nil {
			return v, true
		}
	}
	def := r.schema[name].Default
	if def != nil {
		return def, true
	}
	return nil, false
}

func (r *presetFieldReader) readCommand(name string) string {
	val, ok := r.rawOrDefault(name)
	if !ok {
		r.addError(name, "required", "command is required")
		return ""
	}
	s, isStr := val.(string)
	if !isStr {
		r.addError(name, "invalid_type", "command must be a string")
		return ""
	}
	if !isValidPresetCommand(s) {
		r.addError(name, "invalid_value", "command must match ^[a-z][a-z0-9_-]{0,31}$ (without a leading slash)")
		return ""
	}
	return s
}

func (r *presetFieldReader) readEnum(name string) string {
	f := r.schema[name]
	val, ok := r.rawOrDefault(name)
	if !ok {
		if f.Required {
			r.addError(name, "required", "field is required")
		}
		return ""
	}
	s, isStr := val.(string)
	if !isStr {
		r.addError(name, "invalid_type", "field must be an enum string")
		return ""
	}
	for _, allowed := range f.Enum {
		if s == allowed {
			return s
		}
	}
	r.addError(name, "invalid_value", "value is not one of the allowed enum options: "+strings.Join(f.Enum, ", "))
	return ""
}

func (r *presetFieldReader) readBool(name string) bool {
	val, ok := r.rawOrDefault(name)
	if !ok {
		return false
	}
	b, isBool := val.(bool)
	if !isBool {
		r.addError(name, "invalid_type", "field must be a boolean")
		return false
	}
	return b
}

func (r *presetFieldReader) readCoalesceWindow(name string) (string, time.Duration) {
	val, ok := r.rawOrDefault(name)
	if !ok {
		return "0s", 0
	}
	s, isStr := val.(string)
	if !isStr {
		r.addError(name, "invalid_type", "coalesce_window must be a duration string (for example 30s)")
		return "", 0
	}
	if s == "" || hasDisallowedTextRunes(s) {
		r.addError(name, "invalid_value", "coalesce_window must be a whole-second Go duration between 0s and 10m")
		return "", 0
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 || d > MaxCoalesceWindow || d%time.Second != 0 {
		r.addError(name, "invalid_value", "coalesce_window must be a whole-second Go duration between 0s and 10m")
		return "", 0
	}
	if d == 0 {
		return "0s", 0
	}
	return d.String(), d
}

func (r *presetFieldReader) readStringSlice(name string) ([]string, bool) {
	val, ok := r.rawOrDefault(name)
	if !ok {
		if r.schema[name].Required {
			r.addError(name, "required", "list field is required")
			return nil, false
		}
		return []string{}, true
	}
	switch typed := val.(type) {
	case []string:
		if len(typed) > maxPresetListItems {
			r.addError(name, "too_many_items", fmt.Sprintf("list exceeds maximum of %d items", maxPresetListItems))
			return nil, false
		}
		return append([]string(nil), typed...), true
	case []any:
		if len(typed) > maxPresetListItems {
			r.addError(name, "too_many_items", fmt.Sprintf("list exceeds maximum of %d items", maxPresetListItems))
			return nil, false
		}
		out := make([]string, 0, len(typed))
		for _, elem := range typed {
			s, isStr := elem.(string)
			if !isStr {
				r.addError(name, "invalid_type", "every list element must be a string")
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	default:
		r.addError(name, "invalid_type", "field must be a list of strings")
		return nil, false
	}
}

func (r *presetFieldReader) readEnumList(name string, requireNonEmpty bool) []string {
	items, ok := r.readStringSlice(name)
	if !ok {
		return nil
	}
	if requireNonEmpty && len(items) == 0 {
		r.addError(name, "empty_list", "at least one value is required")
		return nil
	}
	allowedSet := make(map[string]bool, len(r.schema[name].Enum))
	for _, a := range r.schema[name].Enum {
		allowedSet[a] = true
	}
	dedup := make([]string, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		if !allowedSet[item] {
			r.addError(name, "invalid_value", "list contains an unsupported value; allowed: "+strings.Join(r.schema[name].Enum, ", "))
			return nil
		}
		if !seen[item] {
			seen[item] = true
			dedup = append(dedup, item)
		}
	}
	return dedup
}

func (r *presetFieldReader) readBaseBranches(name string) []string {
	items, ok := r.readStringSlice(name)
	if !ok {
		return nil
	}
	dedup := make([]string, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		if item != DefaultBaseBranchSentinel {
			if err := validateBranchRefStdlib(item); err != nil {
				r.addError(name, "invalid_value", err.Error())
				return nil
			}
		}
		if !seen[item] {
			seen[item] = true
			dedup = append(dedup, item)
		}
	}
	return dedup
}

func (r *presetFieldReader) readLabel(name string) string {
	val, ok := r.rawOrDefault(name)
	if !ok {
		r.addError(name, "required", "label is required")
		return ""
	}
	s, isStr := val.(string)
	if !isStr {
		r.addError(name, "invalid_type", "label must be a string")
		return ""
	}
	if err := validateGitHubLabel(s); err != nil {
		r.addError(name, "invalid_value", err.Error())
		return ""
	}
	return s
}

func (r *presetFieldReader) readLabelsList(name string) []string {
	items, ok := r.readStringSlice(name)
	if !ok {
		return nil
	}
	dedup := make([]string, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		if err := validateGitHubLabel(item); err != nil {
			r.addError(name, "invalid_value", err.Error())
			return nil
		}
		if !seen[item] {
			seen[item] = true
			dedup = append(dedup, item)
		}
	}
	return dedup
}

func isValidPresetCommand(s string) bool {
	if len(s) < 1 || len(s) > MaxCommandNameRunes {
		return false
	}
	if s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
}

func validateGitHubLabel(s string) error {
	if s == "" {
		return errors.New("label must be non-empty")
	}
	if len(s) > maxPresetLabelBytes {
		return fmt.Errorf("label exceeds maximum length of %d bytes", maxPresetLabelBytes)
	}
	if strings.TrimSpace(s) != s {
		return errors.New("label must not have leading or trailing whitespace")
	}
	if !utf8.ValidString(s) {
		return errors.New("label must be valid UTF-8")
	}
	if hasDisallowedTextRunes(s) {
		return errors.New("label must not contain control, newline, or format characters")
	}
	return nil
}

// validateBranchRefStdlib mirrors pkg/gitref.ValidBranch in pure stdlib so
// package github remains a strict leaf package without in-module imports.
// Returned errors never quote the rejected branch value.
func validateBranchRefStdlib(name string) error {
	if name == "" {
		return errors.New("branch name must not be empty")
	}
	if len(name) > maxPresetBranchBytes {
		return fmt.Errorf("branch name must not exceed %d bytes", maxPresetBranchBytes)
	}
	if strings.Contains(name, "..") || strings.Contains(name, "@{") || strings.Contains(name, "//") {
		return errors.New("branch name must not contain '..', '@{' or '//'")
	}
	if !isASCIIAlnum(name[0]) {
		return errors.New("branch name must start with an ASCII letter or digit")
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !isASCIIAlnum(c) && c != '.' && c != '_' && c != '/' && c != '-' {
			return errors.New("branch name may contain only ASCII letters, digits, '.', '_', '/' and '-'")
		}
	}
	if strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") || strings.HasSuffix(name, ".lock") {
		return errors.New("branch name must not end with '/', '.' or '.lock'")
	}
	for _, seg := range strings.Split(name, "/") {
		if strings.HasPrefix(seg, ".") || strings.HasSuffix(seg, ".lock") {
			return errors.New("branch path components must not start with '.' or end with '.lock'")
		}
	}
	if name == "HEAD" {
		return errors.New("branch name 'HEAD' is reserved")
	}
	if isHexHashString(strings.ToLower(name)) {
		return errors.New("branch name must not be a 40- or 64-character hex commit hash")
	}
	return nil
}

func isASCIIAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func isHexHashString(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func hasDisallowedTextRunes(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
			return true
		}
	}
	return false
}

func sanitizeFieldKeyForError(raw string) string {
	if raw == "" || len(raw) > maxPresetFieldKeyBytes || !utf8.ValidString(raw) {
		return "<invalid>"
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' || c == '.' {
			continue
		}
		return "<invalid>"
	}
	return raw
}
