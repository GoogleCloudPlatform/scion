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
	// provenance, or the idempotency key was used for a different answer.
	ErrDecisionAnswerConflict = errors.New("decision answer conflict")
	// ErrNotRecordedAnswerer: a direct answer to a version that has an
	// agent_interpreted answer, from a user other than the one that answer
	// records. Only the recorded answerer may confirm or override it.
	ErrNotRecordedAnswerer = errors.New("not the recorded answerer")
)

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
	// this provenance or the idempotency key names another answer. A direct
	// answer to a version with an agent_interpreted answer must come from
	// the user that answer records (else ErrNotRecordedAnswerer) and carry
	// outcome confirm (same choice) or override (another choice); every
	// other answer carries outcome answer (else ErrInvalidInput). A retry
	// with the same idempotency key, choice and answerer returns the
	// recorded answer with replayed true and stores nothing. On Postgres
	// the decision row is locked for the transaction, so an answer and a
	// concurrent repost of the key serialize.
	RecordAnswer(ctx context.Context, in *RecordAnswerInput) (answer *DecisionAnswer, replayed bool, err error)

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
