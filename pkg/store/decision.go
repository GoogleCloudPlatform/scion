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
)

// Choices (ptone/scion#7514): an agent's choice prompt carries decisions,
// each stored as one row per version (Decision), and the answers to them
// are the decision log (DecisionAnswer).

// MaxPendingDecisionsPerAgent caps an agent's pending decisions. A prompt
// that would exceed it is refused (ErrTooManyPendingDecisions), never
// evicting older ones. It is a soft cap: concurrent sends may overshoot it
// by a few.
const MaxPendingDecisionsPerAgent = 50

// Decision statuses.
const (
	DecisionStatusPending  = "pending"
	DecisionStatusDecided  = "decided"
	DecisionStatusReplaced = "replaced"
)

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
)

// Decision answer surfaces: where the user acted.
const (
	DecisionSurfaceNative     = "native"
	DecisionSurfaceSlack      = "slack"
	DecisionSurfaceGoogleChat = "google_chat"
	DecisionSurfaceDiscord    = "discord"
	DecisionSurfaceTelegram   = "telegram"
	DecisionSurfaceTeams      = "teams"
	DecisionSurfaceOther      = "other"
)

// MaxDecisionIdempotencyKeyLen bounds RecordAnswerInput.IdempotencyKey.
const MaxDecisionIdempotencyKeyLen = 128

var (
	// ErrChoiceKeyExists: a choice prompt without Update reuses a key that
	// already exists for the agent. The error is a *ChoiceKeyExistsError.
	ErrChoiceKeyExists = errors.New("choice_key_exists")
	// ErrDecisionKeyNotIssued: an all-digit key that the hub never issued
	// to the agent. The error is a *DecisionKeyNotIssuedError.
	ErrDecisionKeyNotIssued = errors.New("decision key not issued")
	// ErrTooManyPendingDecisions: the prompt would leave the agent with
	// more than MaxPendingDecisionsPerAgent pending decisions.
	ErrTooManyPendingDecisions = errors.New("too many pending decisions")
	// ErrDecisionReplaced: the decision version was replaced by a newer
	// one and can no longer be answered.
	ErrDecisionReplaced = errors.New("replaced")
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
	// ErrIdempotencyMismatch: a set submit reused an idempotency key the
	// user already used on the prompt for a different set of answers.
	ErrIdempotencyMismatch = errors.New("idempotency mismatch")
)

// Reasons a row of a set submit is not answerable, or is missing from it
// (AnswerSetChangedError.Reasons).
const (
	AnswerSetReasonReplaced            = "replaced"
	AnswerSetReasonDecided             = "decided"
	AnswerSetReasonNotRecordedAnswerer = "not_recorded_answerer"
	AnswerSetReasonMissing             = "missing"
	AnswerSetReasonNotActionable       = "not_actionable"
)

// AnswerSetChangedError lists, by decision row id, why a set submit does
// not equal the prompt's actionable set: a requested row that is not
// actionable (replaced, decided, not_recorded_answerer, not_actionable) or
// an actionable row the request left out (missing). It matches
// ErrAnswerSetChanged.
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
// decided for a direct answer, not_recorded_answerer for another user's
// interpreted answer). d.Answers must be loaded.
func DecisionActionableReason(d *Decision, userID string) string {
	if d.Status == DecisionStatusReplaced {
		return AnswerSetReasonReplaced
	}
	var interpreted *DecisionAnswer
	for _, a := range d.Answers {
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
}

// Hint is the one-line description of the existing key, for example
// "D234 already exists (v1, decided by ptone: yes)".
func (e ExistingDecisionKey) Hint() string {
	state := e.Status
	if e.Answer != nil {
		who := e.Answer.AnsweredByName
		if who == "" {
			who = e.Answer.AnsweredByEmail
		}
		if who == "" {
			who = "a user"
		}
		state = fmt.Sprintf("decided by %s: %s", who, e.Answer.ChoiceLabel)
	}
	return fmt.Sprintf("%s already exists (v%d, %s)", e.Key, e.Version, state)
}

// ChoiceKeyExistsError lists every key of a refused prompt that already
// exists. Nothing was stored. It matches ErrChoiceKeyExists.
type ChoiceKeyExistsError struct {
	Existing []ExistingDecisionKey
}

func (e *ChoiceKeyExistsError) Error() string {
	parts := make([]string, len(e.Existing))
	for i, k := range e.Existing {
		parts[i] = k.Hint()
	}
	return strings.Join(parts, "; ") + "; pass --update to replace it, or use a new id"
}

func (e *ChoiceKeyExistsError) Unwrap() error { return ErrChoiceKeyExists }

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
	DefaultChoiceID string // empty when none

	// IntroLength is set on the message's position 0 row only: the byte
	// offset in the prompt's msg where the markdown fallback starts
	// (messages.ChoiceIntro recovers the intro).
	IntroLength int

	Status       string // DecisionStatus*
	ReplacedByID string // empty unless replaced

	CreatedAt  time.Time
	DecidedAt  *time.Time
	ReplacedAt *time.Time

	// Answers are the version's answers, oldest first. GetDecision and
	// GetDecisionsByMessageIDs load them.
	Answers []*DecisionAnswer
}

// Ref is the decision's global name across versions: "<agent-uuid>/<key>".
func (d *Decision) Ref() string { return d.AskerAgentID + "/" + d.Key }

// EffectiveAnswer is the direct answer if there is one, else the
// interpreted one, else nil.
func (d *Decision) EffectiveAnswer() *DecisionAnswer {
	var interpreted *DecisionAnswer
	for _, a := range d.Answers {
		switch a.Provenance {
		case DecisionProvenanceDirect:
			return a
		case DecisionProvenanceAgentInterpreted:
			interpreted = a
		}
	}
	return interpreted
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

	// AnsweredByUserID is empty after the user is deleted; the name and
	// email snapshots stay.
	AnsweredByUserID string
	AnsweredByName   string
	AnsweredByEmail  string

	Provenance           string // DecisionProvenance*
	Outcome              string // DecisionOutcome*
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

// ChoicePromptInput is a choice prompt to store: its message and its
// decisions, normalized by messages.NormalizeDecisions.
type ChoicePromptInput struct {
	// Message is the choice-prompt message row. ID and ProjectID are
	// required; Type is set to messages.TypeChoicePrompt and Msg to the
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
	// any existing key is refused as a whole (ChoiceKeyExistsError).
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

// ChoicePromptResult is what CreateChoicePrompt stored.
type ChoicePromptResult struct {
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

	AnsweredByUserID string
	AnsweredByName   string
	AnsweredByEmail  string

	Provenance           string // DecisionProvenance*; required
	Outcome              string // DecisionOutcome*; empty means answer
	InterpretedByAgentID string // required iff agent_interpreted
	SourceMessageID      string // required iff agent_interpreted
	Surface              string // DecisionSurface*; required

	// ResponseMessage is the choice-response message, stored in the same
	// transaction (ID and ProjectID required; Type is set to
	// messages.TypeChoiceResponse).
	ResponseMessage *Message

	AuthorizingUserID string
	DelegationGrantID string
	// IdempotencyKey, when set, makes a retry with the same key, choice
	// and answerer return the recorded answer (replayed) instead of
	// ErrDecisionAnswerConflict. At most MaxDecisionIdempotencyKeyLen.
	IdempotencyKey string
}

// AnswerSetItem is one answer of a set submit: the row, the chosen id and
// the outcome the caller expects from the state it read (answer, or
// confirm or override over the user's own agent_interpreted answer).
type AnswerSetItem struct {
	DecisionRowID string
	ChoiceID      string
	Outcome       string // DecisionOutcome*; empty means answer
}

// RecordAnswerSetInput is one set submit of a choice prompt (design choices
// v5, 4.2): direct answers from the native surface for every actionable
// row of the prompt message, by one user.
type RecordAnswerSetInput struct {
	MessageID string

	AnsweredByUserID string // required
	AnsweredByName   string
	AnsweredByEmail  string

	Answers []AnswerSetItem

	// ResponseMessage is the one choice-response message of the set,
	// stored in the same transaction (ID and ProjectID required; Type is
	// set to messages.TypeChoiceResponse).
	ResponseMessage *Message

	// IdempotencyKey, when set, is scoped to the user and the prompt: a
	// retry with the same key and the same answers returns the recorded
	// set (replayed); the same key with other answers is
	// ErrIdempotencyMismatch. At most MaxDecisionIdempotencyKeyLen.
	IdempotencyKey string
}

// AnswerSetResult is a recorded set submit: its batch id and its answers in
// position order.
type AnswerSetResult struct {
	BatchID  string
	Answers  []*DecisionAnswer
	Replayed bool
}

// DecisionStore persists choice prompt decisions and their answers.
type DecisionStore interface {
	// CreateChoicePrompt stores a choice prompt in one transaction: it
	// assigns the next numbers from the agent's counter to decisions
	// without a key (never reused), refuses an all-digit key the agent
	// was never issued (DecisionKeyNotIssuedError), and without Update
	// refuses a prompt that reuses any existing key, listing each with its
	// version, status and answer (ChoiceKeyExistsError); with Update it
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
	CreateChoicePrompt(ctx context.Context, in *ChoicePromptInput) (*ChoicePromptResult, error)

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
	// with each of the given choice-response messages, keyed by message
	// ID, for history pages. IDs with no answer (or not UUIDs) are absent.
	GetDecisionAnswersByResponseMessageIDs(ctx context.Context, messageIDs []string) (map[string]*DecisionAnswer, error)
}
