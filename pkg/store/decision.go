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

package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/msgaction"
)

// Decisions (ptone/scion#7514): an agent's decision prompt carries decisions,
// each stored as one row per version (Decision), and the answers to them
// are the decision log (DecisionAnswer).

// MaxPendingDecisionsPerAgent caps an agent's pending decisions. A prompt
// that would exceed it is refused (ErrTooManyPendingDecisions), never
// evicting older ones. It is a soft cap: concurrent sends may overshoot it
// by a few.
const MaxPendingDecisionsPerAgent = 50

// Decision statuses.
const (
	DecisionStatusPending   = "pending"
	DecisionStatusDecided   = "decided"
	DecisionStatusReplaced  = "replaced"
	DecisionStatusWithdrawn = "withdrawn"
	DecisionStatusDismissed = "dismissed"
)

// MaxDecisionWithdrawReasonRunes bounds WithdrawDecisionsInput.Reason.
const MaxDecisionWithdrawReasonRunes = msgaction.MaxReasonRunes

// MaxDecisionDismissReasonRunes bounds DismissDecisionsInput.Reason.
const MaxDecisionDismissReasonRunes = msgaction.MaxReasonRunes

// DecisionDismissMinAge is the minimum age a decision version must reach
// before a user may dismiss it (design-8194 section 4.4a, D10).
const DecisionDismissMinAge = msgaction.DecisionDismissMinAge

// DecisionStatusTerminal reports whether status is a terminal decision status
// (replaced, withdrawn, or dismissed) that cannot accept new answers.
func DecisionStatusTerminal(status string) bool {
	cls, ok := msgaction.DecisionKindSpec.Classify(status)
	return ok && cls.Terminal() && cls != msgaction.ClassResolved
}

// Decision key sources.
const (
	DecisionKeySourceAgent = "agent"
	DecisionKeySourceHub   = "hub"
)

// Decision answer provenances.
const (
	DecisionProvenanceDirect           = "direct"
	DecisionProvenanceAgentInterpreted = "agent_interpreted"
)

// Decision answer outcomes.
const (
	DecisionOutcomeAnswer   = "answer"
	DecisionOutcomeConfirm  = "confirm"
	DecisionOutcomeOverride = "override"
	DecisionOutcomeDismiss  = "dismiss"
)

// Decision answer surfaces: where the user acted.
const (
	DecisionSurfaceNative        = "native"
	DecisionSurfaceDecisionsPage = "decisions_page"
	DecisionSurfaceInbox         = "inbox"
	DecisionSurfaceSlack         = "slack"
	DecisionSurfaceGoogleChat    = "google_chat"
	DecisionSurfaceDiscord       = "discord"
	DecisionSurfaceTelegram      = "telegram"
	DecisionSurfaceTeams         = "teams"
	DecisionSurfaceOther         = "other"
)

// MaxDecisionIdempotencyKeyLen bounds RecordAnswerInput.IdempotencyKey.
const MaxDecisionIdempotencyKeyLen = msgaction.MaxIdempotencyKeyLen

var (
	// ErrDecisionKeyExists: a decision prompt without Update reuses a key that
	// already exists for the agent. The error is a *DecisionKeyExistsError.
	ErrDecisionKeyExists = errors.New("decision_key_exists")
	// ErrDecisionKeyNotIssued: an all-digit key that the hub never issued
	// to the agent. The error is a *DecisionKeyNotIssuedError.
	ErrDecisionKeyNotIssued = errors.New("decision key not issued")
	// ErrTooManyPendingDecisions: the prompt would leave the agent with
	// more than MaxPendingDecisionsPerAgent pending decisions.
	ErrTooManyPendingDecisions = errors.New("too many pending decisions")
	// ErrDecisionReplaced: the decision version was replaced by a newer
	// one and can no longer be answered.
	ErrDecisionReplaced = errors.New("replaced")
	// ErrDecisionWithdrawn: the decision version was withdrawn by its
	// asking agent and can no longer be answered.
	ErrDecisionWithdrawn = errors.New("withdrawn")
	// ErrDecisionDismissed: the decision version was dismissed by a user
	// and can no longer be answered.
	ErrDecisionDismissed = errors.New("dismissed")
	// ErrDecisionTooRecent: every open decision of the prompt is younger
	// than DecisionDismissMinAge and cannot be dismissed yet. Matches
	// *DecisionTooRecentError.
	ErrDecisionTooRecent = errors.New("too_recent")
	// ErrDismissChanged: the decisions to dismiss changed or none are open
	// for the user. Matches *DismissChangedError.
	ErrDismissChanged = errors.New("dismiss changed")
	// ErrDecisionAnswerConflict: the version already has an answer of this
	// provenance (an agent_interpreted answer: of either provenance), or
	// the idempotency key was used for a different answer.
	ErrDecisionAnswerConflict = errors.New("decision answer conflict")
	// ErrNotRecordedAnswerer: a direct answer to a version that has an
	// agent_interpreted answer, from a user other than the one that answer
	// records. Only the recorded answerer may confirm or override it.
	ErrNotRecordedAnswerer = errors.New("not the recorded answerer")
	// ErrDecisionOutcomeMismatch: a direct answer's outcome does not fit
	// the version's recorded answers (confirm or override needs an
	// agent_interpreted answer; an answer over one must confirm or
	// override it), typically because the state changed since the caller
	// read it.
	ErrDecisionOutcomeMismatch = errors.New("decision outcome mismatch")
	// ErrAnswerSetChanged: a set submit's rows are not the prompt's
	// actionable set (design choices v5, 4.2 step 5). It matches
	// *AnswerSetChangedError, which names a reason per row.
	ErrAnswerSetChanged = errors.New("answer set changed")
	// ErrIdempotencyMismatch: a set submit or interpreted-answer call reused
	// an idempotency key for a different set of answers.
	ErrIdempotencyMismatch = errors.New("idempotency mismatch")
	// ErrInterpretedAnswersFailed: one or more pairs of RecordInterpretedAnswers
	// failed row-state or choice validation; nothing was stored. Matches
	// *InterpretedAnswersError.
	ErrInterpretedAnswersFailed = errors.New("interpreted answers failed")
	// ErrWithdrawDecisionsFailed: one or more keys of WithdrawDecisions
	// failed lookup or row-state validation; nothing was stored. Matches
	// *WithdrawDecisionsError.
	ErrWithdrawDecisionsFailed = errors.New("withdraw decisions failed")
)

// DecisionListState filters ListAgentDecisions by row status.
type DecisionListState string

const (
	DecisionListStatePending   DecisionListState = "pending"
	DecisionListStateDecided   DecisionListState = "decided"
	DecisionListStateWithdrawn DecisionListState = "withdrawn"
	DecisionListStateDismissed DecisionListState = "dismissed"
	DecisionListStateAll       DecisionListState = "all"

	DefaultDecisionListLimit = 50
	MaxDecisionListLimit     = 200
)

// ProjectDecisionProvenance* values filter ListDecisionAnswers by
// answer provenance + outcome (Plan Phase 3, §5.1; design-8194 §4.3).
const (
	ProjectDecisionProvenanceDirect      = "direct"
	ProjectDecisionProvenanceInterpreted = "interpreted"
	ProjectDecisionProvenanceConfirmed   = "confirmed"
	ProjectDecisionProvenanceOverridden  = "overridden"
	ProjectDecisionProvenanceDismissed   = "dismissed"
)

// ProjectDecisionActor identifies an agent or user referenced by a project's
// decisions or answers, carrying a stored display-name snapshot so deleted
// actors remain readable. ProjectID is populated for asking agents (an agent
// belongs to one project) and empty for answering users (users span projects).
// Email is populated for answering users when recorded on their answers.
type ProjectDecisionActor struct {
	ID        string
	Name      string
	Email     string
	ProjectID string
}

// ProjectDecisionPrompt groups every decision row (in position order, with
// Answers loaded) of one decision-prompt message that has at least one open
// (pending or unconfirmed agent_interpreted) row in the project.
type ProjectDecisionPrompt struct {
	ProjectID              string
	MessageID              string
	ChatKey                string
	AskerAgentID           string
	AskerAgentName         string
	CreatedAt              time.Time
	PromptExistsInMessages bool
	Decisions              []*Decision
}

// ProjectDecisionExcludeQuery is one negated free-text clause for
// ListDecisionAnswers (e.g. `-foo` in structured filter query mode).
type ProjectDecisionExcludeQuery struct {
	Text     string
	ChatKeys []string
}

// ProjectDecisionAnswerFilter filters ListDecisionAnswers (tab=made).
type ProjectDecisionAnswerFilter struct {
	AgentID            string     // asker_agent_id UUID (ANDed with AgentIDs when both are set)
	AgentIDs           []string   // asker_agent_id IN (...)
	ExcludeAgentIDs    []string   // asker_agent_id NOT IN (...)
	UserID             string     // answered_by_user_id (ANDed with UserIDs when both are set)
	UserIDs            []string   // answered_by_user_id IN (...)
	ExcludeUserIDs     []string   // answered_by_user_id NOT IN (...) (NULL rows preserved)
	Provenance         string     // ProjectDecisionProvenance* or "" for all
	Provenances        []string   // OR of ProjectDecisionProvenance* values
	ExcludeProvenances []string   // AND of NOT ProjectDecisionProvenance* values
	From               *time.Time // inclusive lower bound on decision_answers.created_at
	To                 *time.Time // inclusive upper bound on decision_answers.created_at
	// Query, when not empty, keeps only answers whose question text,
	// choice label or decision key, or whose decision's asker agent name,
	// contains it case-insensitively (matched literally: % and _ are not
	// wildcards), or whose decision's chat_key is in QueryChatKeys (the
	// readable conversations whose label matched Query in memory).
	Query          string
	QueryChatKeys  []string
	ExcludeQueries []ProjectDecisionExcludeQuery
}

// ProjectDecisionAnswerEntry is one Made-tab decision_answers entry enriched
// with its associated decision row, folded sibling agent_interpreted answer
// (when a direct confirm/override/dismiss superseded it, design-8443 §4.2),
// awaiting-confirmation flag (when an unconfirmed agent_interpreted row stands
// on an active decision), and message-availability flags against messages.
type ProjectDecisionAnswerEntry struct {
	Answer                 *DecisionAnswer
	Decision               *Decision
	InterpretedAnswer      *DecisionAnswer
	PreviousChoiceLabel    string
	PreviousOtherText      string
	AwaitingConfirmation   bool
	PromptExistsInMessages bool
	SourceExistsInMessages bool
}

// Per-key failure reasons in InterpretedAnswersError.
const (
	InterpretedReasonAlreadyAnswered = "already_answered"
	InterpretedReasonReplaced        = "replaced"
	InterpretedReasonWithdrawn       = "withdrawn"
	InterpretedReasonDismissed       = "dismissed"
	InterpretedReasonInvalidChoice   = "invalid_choice"
	InterpretedReasonNotFound        = "decision_not_found"
)

// InterpretedKeyFailure describes why one pair in RecordInterpretedAnswers
// could not be recorded.
type InterpretedKeyFailure struct {
	Key      string
	Reason   string    // InterpretedReason*
	ChoiceID string    // populated for InterpretedReasonInvalidChoice
	Decision *Decision // current row with Answers loaded, when present
}

// InterpretedAnswersError lists every pair that failed in an all-or-nothing
// RecordInterpretedAnswers call. Nothing was stored.
type InterpretedAnswersError struct {
	Failures []InterpretedKeyFailure
}

func (e *InterpretedAnswersError) Error() string {
	parts := make([]string, len(e.Failures))
	for i, f := range e.Failures {
		switch f.Reason {
		case InterpretedReasonInvalidChoice:
			parts[i] = fmt.Sprintf("%s: invalid choice %q", f.Key, f.ChoiceID)
		default:
			parts[i] = fmt.Sprintf("%s: %s", f.Key, f.Reason)
		}
	}
	return strings.Join(parts, "; ")
}

func (e *InterpretedAnswersError) Unwrap() error { return ErrInterpretedAnswersFailed }

// Per-key failure reasons in WithdrawDecisionsError.
const (
	WithdrawReasonAlreadyAnswered = "already_answered"
	WithdrawReasonDismissed       = "dismissed"
	WithdrawReasonNotFound        = "decision_not_found"
)

// WithdrawKeyFailure describes why one key in WithdrawDecisions could not be
// withdrawn.
type WithdrawKeyFailure struct {
	Key      string
	Reason   string    // WithdrawReason*
	Decision *Decision // current active row with Answers loaded, when present
}

// WithdrawDecisionsError lists every key that failed in an all-or-nothing
// WithdrawDecisions call. Nothing was stored.
type WithdrawDecisionsError struct {
	Failures []WithdrawKeyFailure
}

func (e *WithdrawDecisionsError) Error() string {
	parts := make([]string, len(e.Failures))
	for i, f := range e.Failures {
		parts[i] = fmt.Sprintf("%s: %s", f.Key, f.Reason)
	}
	return strings.Join(parts, "; ")
}

func (e *WithdrawDecisionsError) Unwrap() error { return ErrWithdrawDecisionsFailed }

// Reasons a row of a set submit is not answerable, or is missing from it
// (AnswerSetChangedError.Reasons).
const (
	AnswerSetReasonReplaced            = "replaced"
	AnswerSetReasonWithdrawn           = "withdrawn"
	AnswerSetReasonDismissed           = "dismissed"
	AnswerSetReasonTooRecent           = "too_recent"
	AnswerSetReasonDecided             = "decided"
	AnswerSetReasonNotRecordedAnswerer = "not_recorded_answerer"
	AnswerSetReasonMissing             = "missing"
	AnswerSetReasonNotActionable       = "not_actionable"
)

// DecisionTooRecentError reports the earliest time at which a too-recent
// decision version becomes eligible for dismissal (design-8194 section 4.4a).
type DecisionTooRecentError struct {
	AvailableAt time.Time
}

func (e *DecisionTooRecentError) Error() string {
	return fmt.Sprintf("decision is too recent to dismiss; available at %s", e.AvailableAt.UTC().Format(time.RFC3339))
}

func (e *DecisionTooRecentError) Unwrap() error { return ErrDecisionTooRecent }

// DismissChangedError lists, by decision row id, why a dismiss request could
// not dismiss the prompt's rows. Nothing was stored. Matches ErrDismissChanged.
// AvailableAt is set when any listed row is too recent: the time by which every
// too-recent listed row reaches DecisionDismissMinAge (an explicit subset is
// all-or-nothing, so the latest of them).
type DismissChangedError struct {
	Reasons     map[string]string
	AvailableAt time.Time
}

func (e *DismissChangedError) Error() string {
	ids := make([]string, 0, len(e.Reasons))
	for id := range e.Reasons {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = id + ": " + e.Reasons[id]
	}
	return "the decisions to dismiss changed: " + strings.Join(parts, "; ")
}

func (e *DismissChangedError) Unwrap() error { return ErrDismissChanged }

// AnswerSetChangedError lists, by decision row id, why a set submit does
// not equal the prompt's actionable set: a requested row that is not
// actionable (replaced, withdrawn, decided, not_recorded_answerer,
// not_actionable) or an actionable row the request left out (missing). It
// matches ErrAnswerSetChanged.
type AnswerSetChangedError struct {
	Reasons map[string]string
}

func (e *AnswerSetChangedError) Error() string {
	ids := make([]string, 0, len(e.Reasons))
	for id := range e.Reasons {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = id + ": " + e.Reasons[id]
	}
	return "the answer set changed: " + strings.Join(parts, "; ")
}

func (e *AnswerSetChangedError) Unwrap() error { return ErrAnswerSetChanged }

// DecisionActionableReason is the actionable-set rule of design choices
// v5, 4.1, for one row and user: "" when the user may answer the row in a
// set submit (pending, or decided with only an agent_interpreted answer
// that records this user), else the reason it is excluded (replaced,
// withdrawn, decided for a direct answer, not_recorded_answerer for another
// user's interpreted answer). d.Answers must be loaded.
func DecisionActionableReason(d *Decision, userID string) string {
	if DecisionStatusTerminal(d.Status) {
		switch d.Status {
		case DecisionStatusReplaced:
			return AnswerSetReasonReplaced
		case DecisionStatusWithdrawn:
			return AnswerSetReasonWithdrawn
		case DecisionStatusDismissed:
			return AnswerSetReasonDismissed
		default:
			return AnswerSetReasonNotActionable
		}
	}
	var interpreted *DecisionAnswer
	for _, a := range d.Answers {
		if a.Outcome == DecisionOutcomeDismiss {
			return AnswerSetReasonDismissed
		}
		switch a.Provenance {
		case DecisionProvenanceDirect:
			return AnswerSetReasonDecided
		case DecisionProvenanceAgentInterpreted:
			interpreted = a
		}
	}
	if interpreted != nil {
		if interpreted.AnsweredByUserID == "" || interpreted.AnsweredByUserID != userID {
			return AnswerSetReasonNotRecordedAnswerer
		}
		return ""
	}
	if d.Status != DecisionStatusPending {
		return AnswerSetReasonNotActionable
	}
	return ""
}

// ExistingDecisionKey describes the active version of a key a prompt tried
// to reuse without Update.
type ExistingDecisionKey struct {
	Key     string
	Version int
	Status  string
	// Answer is the effective answer of a decided version, else nil.
	Answer *DecisionAnswer
	// Dismissal is the outcome=dismiss row of a dismissed version, else nil.
	Dismissal *DecisionAnswer
}

// Hint is the one-line description of the existing key, for example
// "D234 already exists (v1, decided by ptone: yes)" or
// "D234 already exists (v1, dismissed by alice)".
func (e ExistingDecisionKey) Hint() string {
	state := e.Status
	switch {
	case e.Answer != nil:
		who := e.Answer.AnsweredByName
		if who == "" {
			who = e.Answer.AnsweredByEmail
		}
		if who == "" {
			who = "a user"
		}
		ansText := e.Answer.ChoiceLabel
		if e.Answer.ChoiceID == messages.ChoiceIDOther && e.Answer.OtherText != "" {
			ansText = fmt.Sprintf("%s %q", e.Answer.ChoiceLabel, e.Answer.OtherText)
		}
		state = fmt.Sprintf("decided by %s: %s", who, ansText)
	case e.Status == DecisionStatusDismissed && e.Dismissal != nil:
		who := e.Dismissal.AnsweredByName
		if who == "" {
			who = e.Dismissal.AnsweredByEmail
		}
		if who == "" {
			who = "a user"
		}
		state = fmt.Sprintf("dismissed by %s", who)
	}
	return fmt.Sprintf("%s already exists (v%d, %s)", e.Key, e.Version, state)
}

// DecisionKeyExistsError lists every key of a refused prompt that already
// exists. Nothing was stored. It matches ErrDecisionKeyExists.
type DecisionKeyExistsError struct {
	Existing []ExistingDecisionKey
}

func (e *DecisionKeyExistsError) Error() string {
	parts := make([]string, len(e.Existing))
	for i, k := range e.Existing {
		parts[i] = k.Hint()
	}
	return strings.Join(parts, "; ") + "; pass --update to replace it, or use a new id"
}

func (e *DecisionKeyExistsError) Unwrap() error { return ErrDecisionKeyExists }

// DecisionKeyNotIssuedError lists the all-digit keys of a refused prompt
// that the hub never issued to the agent. It matches
// ErrDecisionKeyNotIssued.
type DecisionKeyNotIssuedError struct {
	Keys []string
}

func (e *DecisionKeyNotIssuedError) Error() string {
	return fmt.Sprintf("decision id %s was never assigned to you; numeric ids are assigned by the hub, so omit the id to get one or use an id with a letter", strings.Join(e.Keys, ", "))
}

func (e *DecisionKeyNotIssuedError) Unwrap() error { return ErrDecisionKeyNotIssued }

// Decision is one version of a decision (a decisions row).
type Decision struct {
	ID             string
	ProjectID      string
	ConversationID string // empty when the prompt had no conversation
	ChatKey        string // prompt message thread_id; empty on legacy rows whose prompt was purged
	MessageID      string
	Position       int

	AskerAgentID   string
	AskerAgentName string

	Key       string
	KeyNorm   string
	KeySource string // DecisionKeySource*
	Version   int

	Question        string
	Choices         []messages.Choice
	DefaultChoiceID string                // empty when none
	Other           *messages.OtherOption // nil when allow_other is off

	// IntroLength is set on the message's position 0 row only: the byte
	// offset in the prompt's msg where the markdown fallback starts
	// (messages.DecisionIntro recovers the intro).
	IntroLength int

	Status       string // DecisionStatus*
	ReplacedByID string // empty unless replaced

	CreatedAt       time.Time
	DecidedAt       *time.Time
	ReplacedAt      *time.Time
	WithdrawnAt     *time.Time
	WithdrawnReason string // empty when none
	DismissedAt     *time.Time

	// Answers are the version's answers, oldest first. GetDecision and
	// GetDecisionsByMessageIDs load them.
	Answers []*DecisionAnswer
}

// Ref is the decision's global name across versions: "<agent-uuid>/<key>".
func (d *Decision) Ref() string { return d.AskerAgentID + "/" + d.Key }

// EffectiveAnswer is the direct answer if there is one, else the
// interpreted one, else nil. Dismissed decisions and outcome=dismiss rows
// never yield an effective answer (design-8194 section 4.1).
func (d *Decision) EffectiveAnswer() *DecisionAnswer {
	if d.Status == DecisionStatusDismissed {
		return nil
	}
	var interpreted *DecisionAnswer
	for _, a := range d.Answers {
		if a.Outcome == DecisionOutcomeDismiss {
			return nil
		}
		switch a.Provenance {
		case DecisionProvenanceDirect:
			return a
		case DecisionProvenanceAgentInterpreted:
			interpreted = a
		}
	}
	return interpreted
}

// Dismissal returns the outcome=dismiss answer row recorded for a dismissed
// decision, or nil when the decision has not been dismissed.
func (d *Decision) Dismissal() *DecisionAnswer {
	for _, a := range d.Answers {
		if a.Outcome == DecisionOutcomeDismiss {
			return a
		}
	}
	return nil
}

// Choice returns the choice with id, or false.
func (d *Decision) Choice(id string) (messages.Choice, bool) {
	for _, c := range d.Choices {
		if c.ID == id {
			return c, true
		}
	}
	return messages.Choice{}, false
}

// DecisionAnswer is one decision_answers row.
type DecisionAnswer struct {
	ID            string
	DecisionRowID string
	ProjectID     string

	// Snapshots of the decision version answered.
	AskerAgentID string
	Key          string
	Version      int
	QuestionText string
	ChoiceID     string
	ChoiceLabel  string
	OtherText    string // non-empty iff ChoiceID == messages.ChoiceIDOther

	// AnsweredByUserID is empty after the user is deleted; the name and
	// email snapshots stay.
	AnsweredByUserID string
	AnsweredByName   string
	AnsweredByEmail  string

	Provenance           string // DecisionProvenance*
	Outcome              string // DecisionOutcome*
	DismissReason        string // optional, <= 500 runes, set iff Outcome == DecisionOutcomeDismiss
	InterpretedByAgentID string // set iff agent_interpreted
	SourceMessageID      string // set iff agent_interpreted
	Surface              string // DecisionSurface*
	ResponseMessageID    string

	AuthorizingUserID string
	DelegationGrantID string
	IdempotencyKey    string

	// BatchID is shared by every answer of one set submit; empty for a
	// one-click answer. MatchedDefault is set when ChoiceID is the
	// decision's recommended choice.
	BatchID        string
	MatchedDefault bool

	CreatedAt time.Time
}

// DecisionPromptInput is a decision prompt to store: its message and its
// decisions, normalized by messages.NormalizeDecisions.
type DecisionPromptInput struct {
	// Message is the decision-prompt message row. ID and ProjectID are
	// required; Type is set to messages.TypeDecisionPrompt and Msg to the
	// rendered markdown fallback (Intro followed by the decisions).
	Message *Message
	// Intro is the agent's body shown above the decisions; may be empty.
	Intro string
	// AnswerURL, when set, is the link the fallback offers for answering
	// in Scion.
	AnswerURL string

	AskerAgentID   string
	AskerAgentName string

	Decisions []messages.Decision
	// Update allows keys that already exist: each gets a new version and
	// its active version is marked replaced. Without it, a prompt reusing
	// any existing key is refused as a whole (DecisionKeyExistsError).
	Update bool
}

// ReplacedDecision is the version a prompt replaced, as it was before.
type ReplacedDecision struct {
	RowID   string
	Version int
	Status  string // the status before it was replaced: pending or decided
	// Answer is its effective answer when it was decided, else nil. The
	// answer row is kept.
	Answer *DecisionAnswer
}

// DecisionResult is one stored decision of a prompt, in message order.
type DecisionResult struct {
	RowID     string
	Key       string
	KeySource string
	Version   int
	// Replaced is the version this one replaced, nil for a new key.
	Replaced *ReplacedDecision
}

// Ref is "<agent-uuid>/<key>" for the result's agent.
func (r DecisionResult) Ref(askerAgentID string) string { return askerAgentID + "/" + r.Key }

// DecisionPromptResult is what CreateDecisionPrompt stored.
type DecisionPromptResult struct {
	Decisions []DecisionResult
	// IntroLength is the stored intro offset (see Decision.IntroLength).
	IntroLength int
}

// RecordAnswerInput is one answer to record. The store copies the key,
// version, question and label from the decision row; the caller supplies
// only the choice id and who answered, where and how.
type RecordAnswerInput struct {
	DecisionRowID string
	ChoiceID      string
	OtherText     string // required iff ChoiceID == messages.ChoiceIDOther; trimmed by store

	AnsweredByUserID string
	AnsweredByName   string
	AnsweredByEmail  string

	Provenance           string // DecisionProvenance*; required
	Outcome              string // DecisionOutcome*; empty means answer
	InterpretedByAgentID string // required iff agent_interpreted
	SourceMessageID      string // required iff agent_interpreted
	Surface              string // DecisionSurface*; required

	// ResponseMessage is the decision-response message, stored in the same
	// transaction (ID and ProjectID required; Type is set to
	// messages.TypeDecisionResponse).
	ResponseMessage *Message

	AuthorizingUserID string
	DelegationGrantID string
	// IdempotencyKey, when set, makes a retry with the same key, choice
	// and answerer return the recorded answer (replayed) instead of
	// ErrDecisionAnswerConflict. At most MaxDecisionIdempotencyKeyLen.
	IdempotencyKey string
}

// AnswerSetItem is one answer of a set submit: the row, the chosen id,
// optional free-form other text (required iff ChoiceID == messages.ChoiceIDOther),
// and the outcome the caller expects from the state it read (answer, or
// confirm or override over the user's own agent_interpreted answer).
type AnswerSetItem struct {
	DecisionRowID string
	ChoiceID      string
	OtherText     string // required iff ChoiceID == messages.ChoiceIDOther; trimmed by store
	Outcome       string // DecisionOutcome*; empty means answer
}

// RecordAnswerSetInput is one set submit of a decision prompt (design choices
// v5, 4.2): direct answers from the native surface for every actionable
// row of the prompt message, by one user.
type RecordAnswerSetInput struct {
	MessageID string

	AnsweredByUserID string // required
	AnsweredByName   string
	AnsweredByEmail  string
	Surface          string // DecisionSurface*; empty defaults to DecisionSurfaceNative

	Answers []AnswerSetItem

	// ResponseMessage is the one decision-response message of the set,
	// stored in the same transaction (ID and ProjectID required; Type is
	// set to messages.TypeDecisionResponse).
	ResponseMessage *Message

	// IdempotencyKey, when set, is scoped to the user and the prompt: a
	// retry with the same key and the same answers returns the recorded
	// set (replayed); the same key with other answers is
	// ErrIdempotencyMismatch. At most MaxDecisionIdempotencyKeyLen.
	IdempotencyKey string
}

// AnswerSetResult is a recorded set submit or interpreted-answer call: its
// batch id (empty for a single-pair interpreted call) and its answers in
// request/position order.
type AnswerSetResult struct {
	BatchID  string
	Answers  []*DecisionAnswer
	Replayed bool
}

// InterpretedAnswerPair is one decision answer in RecordInterpretedInput.
type InterpretedAnswerPair struct {
	// DecisionRowID, when set, names the resolved version row to answer;
	// when empty, Key is resolved to the asking agent's active version.
	DecisionRowID string
	Key           string
	ChoiceID      string
	OtherText     string // required iff ChoiceID == messages.ChoiceIDOther; trimmed by store
}

// InterpretedFormattedItem carries the canonical key, choice label, and
// optional Other text of one validated pair so
// RecordInterpretedInput.FormatMessage can render the single decision-response
// message text inside the transaction.
type InterpretedFormattedItem struct {
	Key         string
	ChoiceID    string
	ChoiceLabel string
	OtherText   string
}

// RecordInterpretedInput records 1 to MaxDecisionsPerMessage agent-interpreted
// answers from one user free-text reply (design choices v5, 7; plan phase 2,
// 5.1).
type RecordInterpretedInput struct {
	InterpretedByAgentID string // required (the asking agent UUID)
	SourceMessageID      string // required (UUID of the user's reply)

	AnsweredByUserID string // required
	AnsweredByName   string
	AnsweredByEmail  string
	Surface          string // DecisionSurface*; required

	Answers []InterpretedAnswerPair

	// ResponseMessage is the one decision-response message stored in the same
	// transaction (ID and ProjectID required; Type is set to
	// messages.TypeDecisionResponse). When FormatMessage is non-nil and Msg is
	// empty, Msg is populated from the validated pairs before insertion.
	ResponseMessage *Message
	FormatMessage   func(items []InterpretedFormattedItem) string

	// IdempotencyKey, when set, is scoped to (InterpretedByAgentID,
	// IdempotencyKey): a retry with the same source message and key=choice
	// pairs returns the recorded answers with Replayed true; reusing the key
	// for different inputs returns ErrIdempotencyMismatch.
	IdempotencyKey string
}

// WithdrawDecisionsInput withdraws open decisions of one asking agent.
type WithdrawDecisionsInput struct {
	AskerAgentID string
	Keys         []string // 1..MaxDecisionsPerMessage; resolved to active versions
	WholeSet     bool     // also every other pending row of each key's prompt message
	Reason       string   // optional, <= 500 runes, trimmed
}

// WithdrawDecisionsResult reports the decisions withdrawn (or already
// withdrawn) by WithdrawDecisions.
type WithdrawDecisionsResult struct {
	Withdrawn        []*Decision // now withdrawn, prompt order, answers loaded
	AlreadyWithdrawn []*Decision
}

// DecisionAnswerer identifies the user recording a dismissal. Dismissal is
// open only to users acting for themselves (session or user access token);
// agents, including delegated agent credentials, are refused, so a dismissal
// row carries no delegation attribution.
type DecisionAnswerer struct {
	UserID string
	Name   string
	Email  string
}

// DismissDecisionsInput dismisses open decisions of one prompt for one user
// (design-8194 section 4.2).
type DismissDecisionsInput struct {
	MessageID       string           // the decision-prompt message
	RowIDs          []string         // optional subset; empty = every row of the prompt waiting on the user
	User            DecisionAnswerer // id, name, email
	Reason          string           // optional, <= 500 runes, trimmed
	Surface         string           // native | decisions_page | inbox
	IdempotencyKey  string           // optional
	ResponseMessage *Message         // the decision-response to store (built by the hub)
	FormatMessage   func(dismissed []*Decision) string
	Now             func() time.Time // optional clock override for tests; defaults to time.Now
	// MinAge overrides DecisionDismissMinAge when positive (test-mode hubs
	// only; <= 0 enforces the production 5-minute DecisionDismissMinAge).
	MinAge time.Duration
}

// DismissDecisionsResult reports the decisions dismissed (or already dismissed)
// by DismissDecisions.
type DismissDecisionsResult struct {
	Dismissed        []*Decision // now dismissed, prompt order, answers loaded
	AlreadyDismissed []*Decision
	BatchID          string
	Replayed         bool // idempotency replay: nothing new stored
}

// DecisionStore persists decision prompt decisions and their answers.
type DecisionStore interface {
	// CreateDecisionPrompt stores a decision prompt in one transaction: it
	// assigns the next numbers from the agent's counter to decisions
	// without a key (never reused), refuses an all-digit key the agent
	// was never issued (DecisionKeyNotIssuedError), and without Update
	// refuses a prompt that reuses any existing key, listing each with its
	// version, status and answer (DecisionKeyExistsError); with Update it
	// marks each reused key's active version replaced and inserts the next
	// version. It then refuses a prompt that would leave the agent more
	// than MaxPendingDecisionsPerAgent pending decisions
	// (ErrTooManyPendingDecisions), renders the markdown fallback into the
	// message's Msg, and inserts the message and the decision rows. On any
	// error nothing is stored. Concurrent reposts of one key serialize on
	// the (agent, key, version) unique index (and, on Postgres, on the
	// lock of the key's rows) and a loser retries, a bounded number of
	// times.
	// Called inside WithTx it joins that transaction and does not retry.
	CreateDecisionPrompt(ctx context.Context, in *DecisionPromptInput) (*DecisionPromptResult, error)

	// GetDecision returns one decision version with its answers.
	// ErrNotFound if it does not exist.
	GetDecision(ctx context.Context, rowID string) (*Decision, error)

	// GetDecisionsByMessageIDs returns the decisions of the given prompt
	// messages with their answers, keyed by message ID, each list in
	// message order. Messages without decisions and malformed IDs are
	// absent.
	GetDecisionsByMessageIDs(ctx context.Context, messageIDs []string) (map[string][]*Decision, error)

	// RecordAnswer records an answer in one transaction with the
	// decision's status change (a pending version becomes decided) and the
	// insert of the response message. ErrNotFound for an unknown row,
	// ErrInvalidInput for an unknown choice or malformed input,
	// ErrDecisionReplaced for a replaced version, and
	// ErrDecisionAnswerConflict when the version already has an answer of
	// this provenance, an agent_interpreted answer finds any answer, or the
	// idempotency key names another answer. A direct answer to a version
	// with an agent_interpreted answer must come from the user that answer
	// records (else ErrNotRecordedAnswerer) and carry outcome confirm (same
	// choice) or override (another choice); every other direct answer
	// carries outcome answer (else ErrDecisionOutcomeMismatch), and an
	// agent_interpreted answer always does (else ErrInvalidInput). A retry
	// with the same idempotency key, choice and answerer returns the
	// recorded answer with replayed true and stores nothing. On Postgres
	// the decision row is locked for the transaction, so an answer and a
	// concurrent repost of the key serialize.
	RecordAnswer(ctx context.Context, in *RecordAnswerInput) (answer *DecisionAnswer, replayed bool, err error)

	// RecordAnswerSet records a set submit in one transaction: it locks
	// the prompt's decision rows (on Postgres), answers a replay of the
	// idempotency key first, checks the shape (ErrInvalidInput for an
	// empty or duplicated answer, a row not in the prompt or a choice not
	// in its row), recomputes the actionable set and requires the request
	// rows to equal it (*AnswerSetChangedError, nothing stored), checks
	// each outcome against the row's answers (a difference is a changed
	// set, reason decided), then stores the response message, one direct
	// native answer per row with one shared batch id and matched_default,
	// and marks pending rows decided. ErrNotFound for a message with no
	// decisions.
	RecordAnswerSet(ctx context.Context, in *RecordAnswerSetInput) (*AnswerSetResult, error)

	// GetDecisionRowsByIDs returns the given decision versions keyed by
	// row ID, without their answers, in one query (history pages read the
	// versions that replaced theirs and the prompts their responses
	// answer). Unknown and malformed IDs are absent.
	GetDecisionRowsByIDs(ctx context.Context, rowIDs []string) (map[string]*Decision, error)

	// GetDecisionAnswersByResponseMessageIDs returns the answer recorded
	// with each of the given decision-response messages, keyed by message
	// ID, for history pages. IDs with no answer (or not UUIDs) are absent.
	GetDecisionAnswersByResponseMessageIDs(ctx context.Context, messageIDs []string) (map[string][]*DecisionAnswer, error)

	// ListAgentDecisions lists decision rows belonging to agentID
	// (asker_agent_id = agentID) filtered by state (pending by default;
	// decided; or all) and optional query (when non-empty, case-insensitive
	// literal substring over the question text, key, or effective answer's
	// choice label), newest first by (created_at DESC, id DESC) with opaque
	// cursor pagination (default limit 50, max 200). Answers are loaded on
	// every returned row.
	ListAgentDecisions(ctx context.Context, agentID string, state DecisionListState, query string, cursor string, limit int) (items []*Decision, nextCursor string, err error)

	// GetAgentDecisionVersions returns all versions of key belonging to
	// agentID (asker_agent_id = agentID, key_norm = DecisionKeyNorm(key)),
	// oldest first (version ASC), with EVERY DecisionAnswer row loaded on
	// each version. Returns ErrNotFound when the agent has no decision with
	// that key.
	GetAgentDecisionVersions(ctx context.Context, agentID, key string) ([]*Decision, error)

	// FindInterpretedReplay checks whether idempotencyKey has already been
	// used by agentID for agent_interpreted answers, without locking or
	// writing. Returns (replay, nil) with Replayed=true and answers ordered
	// canonically by (prompt CreatedAt, MessageID, Position) when the key
	// matches sourceMessageID and every pair in pairs; (nil,
	// ErrIdempotencyMismatch) when the key was already used by agentID for a
	// different source message or different answers; and (nil, nil) when
	// idempotencyKey is empty or unused by agentID.
	FindInterpretedReplay(ctx context.Context, agentID, idempotencyKey, sourceMessageID string, pairs []InterpretedAnswerPair) (*AnswerSetResult, error)

	// RecordInterpretedAnswers atomically records 1..MaxDecisionsPerMessage
	// agent_interpreted answers for one asking agent from one user reply
	// message: it checks idempotency scoped to (agent, idempotency_key)
	// first (and re-checks after row locks on contention), locks target rows
	// on Postgres in (key_norm, version) order, validates every target row
	// is pending with no existing answer and a valid choice id (returning
	// *InterpretedAnswersError with all per-key failures and storing nothing
	// if any fails), orders validated answers canonically by (prompt
	// CreatedAt, MessageID, Position), inserts one decision-response message,
	// inserts the DecisionAnswer rows (sharing a new BatchID when len >= 2,
	// empty BatchID when len == 1), and marks every target row decided.
	RecordInterpretedAnswers(ctx context.Context, in *RecordInterpretedInput) (*AnswerSetResult, error)

	// WithdrawDecisions withdraws open decisions of one asking agent in one
	// transaction: it locks target rows on Postgres in (key_norm, version)
	// order, validates every key (unknown -> decision_not_found; any answer,
	// direct or interpreted awaiting confirm -> already_answered; already
	// withdrawn -> AlreadyWithdrawn idempotently without overwriting the first
	// reason), returns *WithdrawDecisionsError with nothing stored if any key
	// fails, and otherwise marks pending target rows withdrawn.
	WithdrawDecisions(ctx context.Context, in *WithdrawDecisionsInput) (*WithdrawDecisionsResult, error)

	// DismissDecisions dismisses open decisions of one prompt for one user in
	// one transaction: it locks the prompt's rows on Postgres in (key_norm,
	// version) order, enforces DecisionDismissMinAge (returning
	// *DecisionTooRecentError when only too-recent open rows exist), stores the
	// decision-response message, marks dismissed rows status=dismissed with
	// dismissed_at, and inserts one outcome=dismiss DecisionAnswer row per
	// dismissed decision with choice_id "$dismissed". Repeating a call when all
	// rows are already dismissed returns them under AlreadyDismissed and stores
	// nothing new.
	DismissDecisions(ctx context.Context, in *DismissDecisionsInput) (*DismissDecisionsResult, error)

	// ListProjectDecisionAgents returns the distinct asking agents recorded on
	// decisions in projectID (including hard-deleted agents whose rows in the
	// agents table are gone), with their newest stored asker_agent_name.
	ListProjectDecisionAgents(ctx context.Context, projectID string) ([]ProjectDecisionActor, error)

	// ListProjectDecisionAgentsForProjects returns, keyed by project ID, the
	// distinct asking agents recorded on decisions in each projectID (including
	// hard-deleted agents), with their newest stored asker_agent_name. Projects
	// with no decisions are omitted. Empty projectIDs returns (nil, nil).
	ListProjectDecisionAgentsForProjects(ctx context.Context, projectIDs []string) (map[string][]ProjectDecisionActor, error)

	// ListOpenPrompts returns every decision prompt in projectIDs whose
	// chat_key is in chatKeys and that still has at least one non-settled
	// decision row (status == pending, or status == decided with no direct
	// answer). Chat keys are unique across the hub, so chatKeys may be the
	// union of several projects' readable keys. Every returned prompt
	// carries its ProjectID and all sibling Decision rows of its message_id
	// in position order with Answers loaded, plus PromptExistsInMessages,
	// ordered newest first by (CreatedAt DESC, MessageID DESC). Empty
	// projectIDs or chatKeys returns (nil, nil) without querying.
	ListOpenPrompts(ctx context.Context, projectIDs, chatKeys []string) ([]*ProjectDecisionPrompt, error)

	// ListDecisionAnswers lists Made-tab decision_answers entries belonging to
	// decisions in projectIDs with chat_key in chatKeys, excluding any
	// agent_interpreted row superseded by a direct sibling on the same
	// decision_row_id (folding that sibling into Entry.InterpretedAnswer),
	// filtered by filter and ordered newest first by (created_at DESC, id DESC)
	// with keyset cursor pagination. limit must be in [0,
	// MaxDecisionListLimit]; limit == 0 validates inputs and returns (nil,
	// "", nil). Empty projectIDs or chatKeys validates inputs and returns
	// (nil, "", nil).
	ListDecisionAnswers(ctx context.Context, projectIDs, chatKeys []string, filter ProjectDecisionAnswerFilter, cursor string, limit int) (entries []*ProjectDecisionAnswerEntry, nextCursor string, err error)

	// ListDecisionAnswersForProjects is the multi-project variant of
	// ListDecisionAnswers where each project arm is scoped to its own readable
	// keys in chatKeysByProject[projectID] (design-scale §3.3). Projects with
	// no readable keys are skipped. Returns the globally newest page ordered
	// by (created_at DESC, id DESC) with identical cursor semantics.
	ListDecisionAnswersForProjects(ctx context.Context, projectIDs []string, chatKeysByProject map[string][]string, filter ProjectDecisionAnswerFilter, cursor string, limit int) (entries []*ProjectDecisionAnswerEntry, nextCursor string, err error)

	// ListDecisionFacets returns, scoped strictly to decisions/answers in
	// projectIDs whose chat_key is in chatKeys: the distinct asking agents
	// (each with the asker_agent_name of its newest decision), the distinct
	// answering users on non-superseded Made entries (at most 500, sorted by
	// display name then ID), and the distinct chat keys that have at least one
	// decision (a subset of chatKeys, in no particular order).
	ListDecisionFacets(ctx context.Context, projectIDs, chatKeys []string) (agents, answerers []ProjectDecisionActor, convKeys []string, err error)

	// ProjectsWithDecisions returns the distinct project IDs in projectIDs
	// that have at least one row in decisions, ordered by each project's
	// most recent decision (created_at DESC, project_id ASC). Empty
	// projectIDs returns (nil, nil) without querying.
	ProjectsWithDecisions(ctx context.Context, projectIDs []string) ([]string, error)

	// ProjectsWithUnsettledDecisions returns the subset of projectIDs holding
	// at least one decision with settled=false (a superset of "has open
	// decisions"). Empty projectIDs returns (nil, nil) without querying.
	ProjectsWithUnsettledDecisions(ctx context.Context, projectIDs []string) (map[string]bool, error)

	// HasPendingDecisionIndexWork reports whether any background decision
	// indexing work is pending (settled-eligible rows still marked
	// settled=false, or decisions/decision_answers still marked
	// facet_indexed=false).
	HasPendingDecisionIndexWork(ctx context.Context) (bool, error)

	// SweepSettledDecisionsPage marks up to limit settled-eligible decisions
	// with id > afterID (when afterID is non-empty) whose settled hint is still
	// false as settled=true in one transaction. Returns the number of rows
	// updated and the highest decision ID scanned on this page (nextCursor,
	// empty when no matching rows remain after afterID). Callers advance
	// afterID = nextCursor while nextCursor != "" (even on short pages, since
	// Postgres FOR UPDATE SKIP LOCKED may return fewer than limit rows).
	SweepSettledDecisionsPage(ctx context.Context, afterID string, limit int) (updated int, nextCursor string, err error)

	// IndexDecisionFacetsDecisionsPage indexes up to limit decisions with
	// id > afterID (when afterID is non-empty) whose facet_indexed marker is
	// false into decision_facet_entries and flips facet_indexed=true in one
	// transaction. Returns the number of rows indexed and the highest decision
	// ID scanned on this page (empty when no matching rows remain).
	IndexDecisionFacetsDecisionsPage(ctx context.Context, afterID string, limit int) (int, string, error)

	// IndexDecisionFacetsAnswersPage indexes up to limit decision_answers with
	// id > afterID (when afterID is non-empty) whose facet_indexed marker is
	// false into decision_facet_entries (handling superseded agent_interpreted
	// siblings under the decision row lock) and flips facet_indexed=true in
	// one transaction. Returns the number of rows indexed and the highest
	// answer ID scanned on this page (empty when no matching rows remain).
	IndexDecisionFacetsAnswersPage(ctx context.Context, afterID string, limit int) (int, string, error)

	// ReconcileDecisionFacets deletes stale decision_facet_entries rows left
	// behind by older-binary user or project deletions. Returns the number of
	// summary rows deleted.
	ReconcileDecisionFacets(ctx context.Context) (int, error)

	// ListDecisionAnswersPerProject lists up to perProject newest non-superseded
	// Made-tab decision_answers entries per project in projectIDs whose
	// decision's chat_key is in chatKeysByProject[projectID], filtered by
	// filter and ordered within each project by (created_at DESC, id DESC).
	// Projects with no keys or no matching answers are omitted from the
	// returned map. perProject must be in [0, MaxDecisionListLimit]; 0 or
	// empty projectIDs/chatKeysByProject validates inputs and returns
	// (nil, nil).
	ListDecisionAnswersPerProject(ctx context.Context, projectIDs []string, chatKeysByProject map[string][]string, filter ProjectDecisionAnswerFilter, perProject int) (map[string][]*ProjectDecisionAnswerEntry, error)
}
