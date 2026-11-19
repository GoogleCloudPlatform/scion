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

package geminidecisions

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
)

const (
	wireTypeNoul   = "noul"
	wireTypeChoice = "choice"

	confidenceFormulaTolerance = 1e-3
	maxJSONNestingDepth        = 32
	blockReasonUnspecified     = "BLOCK_REASON_UNSPECIFIED"
)

type stringKV struct {
	Key   string
	Value string
}

type orderedStringMap []stringKV

func (m orderedStringMap) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, kv := range m {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := marshalNoEscape(kv.Key)
		if err != nil {
			return nil, err
		}
		vb, err := marshalNoEscape(kv.Value)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

type wireBinaryCriteria struct {
	TrueDescription  string `json:"true_description"`
	FalseDescription string `json:"false_description"`
}

type wireQuestion struct {
	Type         string          `json:"type"`
	Instructions string          `json:"instructions"`
	Criteria     json.RawMessage `json:"criteria,omitempty"`
}

type questionKV struct {
	ID       string
	Question wireQuestion
}

type orderedQuestionMap []questionKV

func (m orderedQuestionMap) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, kv := range m {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := marshalNoEscape(kv.ID)
		if err != nil {
			return nil, err
		}
		qb, err := marshalNoEscape(kv.Question)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		buf.Write(qb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

type wireCreateDecisionRequest struct {
	Model     string             `json:"model"`
	Input     string             `json:"input"`
	Questions orderedQuestionMap `json:"questions"`
}

// EncodeRequest serializes req into a Gemini Developer API v1beta
// CreateDecisionRequest JSON payload (UTF-8 without BOM, no HTML escaping).
//
// Selected input fields are serialized as a compact JSON object in sorted key
// order inside the single top-level string field "input"; untrusted field
// values appear exclusively there (PV3 brief §5.2, AC-2).
func EncodeRequest(model string, req provider.ModelRequest) ([]byte, []spec.Question, error) {
	trimmedModel, err := validateModelIdentifier(model)
	if err != nil {
		return nil, nil, err
	}
	if req.Rule.Form == spec.FormNative {
		return nil, nil, provider.NewProviderError(provider.FailUnsupported, false, 0, 0, "native specs are not supported in PV3")
	}

	selectedQuestions, err := selectRequestQuestions(req.Rule, req.Questions)
	if err != nil {
		return nil, nil, err
	}

	inputs := make(orderedStringMap, 0, len(req.Input))
	seenInputs := make(map[string]struct{}, len(req.Input))
	for _, f := range req.Input {
		if f.Name == "" {
			return nil, nil, provider.NewProviderError(provider.FailInvalidRequest, false, 0, 0, "input field name must not be empty")
		}
		if _, dup := seenInputs[f.Name]; dup {
			return nil, nil, provider.NewProviderError(provider.FailInvalidRequest, false, 0, 0, "duplicate input field name in request")
		}
		seenInputs[f.Name] = struct{}{}
		inputs = append(inputs, stringKV{Key: f.Name, Value: f.Value})
	}
	sort.Slice(inputs, func(i, j int) bool {
		return inputs[i].Key < inputs[j].Key
	})

	inputBytes, err := marshalNoEscape(inputs)
	if err != nil {
		return nil, nil, provider.NewProviderError(provider.FailInvalidRequest, false, 0, 0, "failed to serialize Gemini input JSON")
	}

	wireQs := make(orderedQuestionMap, 0, len(selectedQuestions))
	for _, q := range selectedQuestions {
		wq, qErr := encodeQuestion(q)
		if qErr != nil {
			return nil, nil, qErr
		}
		wireQs = append(wireQs, questionKV{ID: q.ID, Question: wq})
	}

	payload := wireCreateDecisionRequest{
		Model:     trimmedModel,
		Input:     string(inputBytes),
		Questions: wireQs,
	}
	encoded, err := marshalNoEscape(payload)
	if err != nil {
		return nil, nil, provider.NewProviderError(provider.FailInvalidRequest, false, 0, 0, "failed to marshal Gemini decisions request JSON")
	}
	return encoded, selectedQuestions, nil
}

func selectRequestQuestions(rule spec.Spec, subset []string) ([]spec.Question, error) {
	if len(rule.Questions) == 0 {
		return nil, provider.NewProviderError(provider.FailInvalidRequest, false, 0, 0, "rule contains no questions")
	}
	if len(subset) == 0 {
		if len(rule.Questions) > spec.MaxQuestionsPerSpec {
			return nil, provider.NewProviderError(provider.FailInvalidRequest, false, 0, 0, "question count exceeds portable ceiling")
		}
		out := make([]spec.Question, len(rule.Questions))
		copy(out, rule.Questions)
		return out, nil
	}
	if len(subset) > spec.MaxQuestionsPerSpec {
		return nil, provider.NewProviderError(provider.FailInvalidRequest, false, 0, 0, "question subset exceeds portable ceiling")
	}

	byID := make(map[string]spec.Question, len(rule.Questions))
	for _, q := range rule.Questions {
		byID[q.ID] = q
	}
	seen := make(map[string]struct{}, len(subset))
	out := make([]spec.Question, 0, len(subset))
	for _, id := range subset {
		if _, dup := seen[id]; dup {
			return nil, provider.NewProviderError(provider.FailInvalidRequest, false, 0, 0, "duplicate question id in request subset")
		}
		seen[id] = struct{}{}
		q, ok := byID[id]
		if !ok {
			return nil, provider.NewProviderError(provider.FailInvalidRequest, false, 0, 0, "request subset references unknown question id")
		}
		out = append(out, q)
	}
	return out, nil
}

func encodeQuestion(q spec.Question) (wireQuestion, error) {
	if q.ID == "" || q.Instructions == "" {
		return wireQuestion{}, provider.NewProviderError(provider.FailInvalidRequest, false, 0, 0, "question id and instructions must be non-empty")
	}

	switch q.Type {
	case spec.PrimitiveBinary:
		wq := wireQuestion{
			Type:         wireTypeNoul,
			Instructions: q.Instructions,
		}
		if q.Criteria != nil {
			if q.Criteria.True == "" || q.Criteria.False == "" {
				return wireQuestion{}, provider.NewProviderError(provider.FailInvalidRequest, false, 0, 0, "binary criteria must specify non-empty true and false descriptions")
			}
			critBytes, err := marshalNoEscape(wireBinaryCriteria{
				TrueDescription:  q.Criteria.True,
				FalseDescription: q.Criteria.False,
			})
			if err != nil {
				return wireQuestion{}, provider.NewProviderError(provider.FailInvalidRequest, false, 0, 0, "failed to encode binary criteria")
			}
			wq.Criteria = critBytes
		}
		return wq, nil

	case spec.PrimitiveChoice:
		if len(q.Options) < spec.MinChoiceOptions || len(q.Options) > spec.MaxChoiceOptions {
			return wireQuestion{}, provider.NewProviderError(provider.FailInvalidRequest, false, 0, 0, "choice option count outside allowed bounds")
		}
		crit := make(orderedStringMap, 0, len(q.Options))
		seenOpts := make(map[string]struct{}, len(q.Options))
		for _, opt := range q.Options {
			if opt.ID == "" || opt.Description == "" {
				return wireQuestion{}, provider.NewProviderError(provider.FailInvalidRequest, false, 0, 0, "choice option id and description are required")
			}
			if _, dup := seenOpts[opt.ID]; dup {
				return wireQuestion{}, provider.NewProviderError(provider.FailInvalidRequest, false, 0, 0, "duplicate choice option id")
			}
			seenOpts[opt.ID] = struct{}{}
			crit = append(crit, stringKV{Key: opt.ID, Value: opt.Description})
		}
		critBytes, err := marshalNoEscape(crit)
		if err != nil {
			return wireQuestion{}, provider.NewProviderError(provider.FailInvalidRequest, false, 0, 0, "failed to encode choice criteria")
		}
		return wireQuestion{
			Type:         wireTypeChoice,
			Instructions: q.Instructions,
			Criteria:     critBytes,
		}, nil

	default:
		return wireQuestion{}, provider.NewProviderError(provider.FailUnsupported, false, 0, 0, "question primitive is not supported by gemini_decisions in PV3")
	}
}

type wireCreateDecisionResponse struct {
	Answers        map[string]json.RawMessage `json:"answers"`
	Usage          *wireUsage                 `json:"usage"`
	PromptFeedback *wirePromptFeedback        `json:"prompt_feedback"`
}

type wireUsage struct {
	TotalInputTokens  *int `json:"total_input_tokens"`
	TotalOutputTokens *int `json:"total_output_tokens"`
}

type wirePromptFeedback struct {
	BlockReason *string `json:"block_reason"`
}

type wireAnswer struct {
	Type          string              `json:"type"`
	Noul          *float64            `json:"noul"`
	Choice        *string             `json:"choice"`
	Confidence    *float64            `json:"confidence"`
	Probabilities map[string]*float64 `json:"probabilities"`
}

// DecodeResponse validates and decodes a 2xx POST /v1beta/decisions response
// body according to PV3 brief §5.3 and provider-design §6.3.
//
// Gemini Decisions omits a response model identifier, so DecodeResponse always
// leaves ResolvedModel empty ("") and attaches NoteModelUnreported. When usage
// or total_input_tokens is absent, DecodeResponse attaches NoteUsageUnreported
// rather than presenting zero tokens as measured usage.
func DecodeResponse(questions []spec.Question, httpStatus int, body []byte) (provider.ModelResult, error) {
	if err := checkNoDuplicateJSONKeys(body); err != nil {
		return provider.ModelResult{}, provider.NewProviderError(provider.FailMalformedResponse, false, 0, httpStatus, "malformed or duplicate-key JSON in provider response")
	}

	var raw wireCreateDecisionResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return provider.ModelResult{}, provider.NewProviderError(provider.FailMalformedResponse, false, 0, httpStatus, "failed to decode provider JSON response")
	}

	// 200 safety-block form (PV3 brief §§1, 5.3, 5.4): prompt_feedback.block_reason
	// maps to content_blocked and never yields a partial result, even if answers
	// is non-empty.
	if raw.PromptFeedback != nil && raw.PromptFeedback.BlockReason != nil {
		br := strings.TrimSpace(*raw.PromptFeedback.BlockReason)
		if br != "" && !strings.EqualFold(br, blockReasonUnspecified) {
			return provider.ModelResult{}, provider.NewProviderError(
				provider.FailContentBlocked,
				false,
				0,
				httpStatus,
				fmt.Sprintf("provider blocked content (HTTP %d)", httpStatus),
			)
		}
	}

	if raw.Answers == nil {
		return provider.ModelResult{}, provider.NewProviderError(provider.FailMalformedResponse, false, 0, httpStatus, "provider response missing required answers object")
	}
	if len(raw.Answers) != len(questions) {
		return provider.ModelResult{}, provider.NewProviderError(provider.FailMalformedResponse, false, 0, httpStatus, "provider answers key count does not match requested questions")
	}

	answers := make(map[string]provider.Answer, len(questions))
	for _, q := range questions {
		rawAns, ok := raw.Answers[q.ID]
		if !ok {
			return provider.ModelResult{}, provider.NewProviderError(provider.FailMalformedResponse, false, 0, httpStatus, "provider response missing requested question answer")
		}
		decoded, err := decodeQuestionAnswer(q, httpStatus, rawAns)
		if err != nil {
			return provider.ModelResult{}, err
		}
		answers[q.ID] = decoded
	}

	resultNotes := []string{provider.NoteModelUnreported}
	var usage provider.Usage
	if raw.Usage == nil || raw.Usage.TotalInputTokens == nil {
		resultNotes = append(resultNotes, provider.NoteUsageUnreported)
	} else {
		if *raw.Usage.TotalInputTokens < 0 {
			return provider.ModelResult{}, provider.NewProviderError(provider.FailMalformedResponse, false, 0, httpStatus, "provider response reported negative input token count")
		}
		outTokens := 0
		if raw.Usage.TotalOutputTokens != nil {
			if *raw.Usage.TotalOutputTokens < 0 {
				return provider.ModelResult{}, provider.NewProviderError(provider.FailMalformedResponse, false, 0, httpStatus, "provider response reported negative output token count")
			}
			outTokens = *raw.Usage.TotalOutputTokens
		}
		usage = provider.Usage{
			InputTokens:  *raw.Usage.TotalInputTokens,
			OutputTokens: outTokens,
		}
	}

	return provider.ModelResult{
		Answers:       answers,
		ResolvedModel: "",
		HTTPStatus:    httpStatus,
		Usage:         usage,
		Notes:         resultNotes,
	}, nil
}

func decodeQuestionAnswer(q spec.Question, httpStatus int, rawAns json.RawMessage) (provider.Answer, error) {
	trimmed := bytes.TrimSpace(rawAns)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return provider.Answer{}, provider.NewProviderError(provider.FailMalformedResponse, false, 0, httpStatus, "provider question answer must be a JSON object")
	}
	var wa wireAnswer
	if err := json.Unmarshal(rawAns, &wa); err != nil {
		return provider.Answer{}, provider.NewProviderError(provider.FailMalformedResponse, false, 0, httpStatus, "invalid question answer payload")
	}

	switch q.Type {
	case spec.PrimitiveBinary:
		if wa.Type != wireTypeNoul {
			return provider.Answer{}, provider.NewProviderError(provider.FailMalformedResponse, false, 0, httpStatus, "binary answer type must be \"noul\"")
		}
		if wa.Noul == nil {
			return provider.Answer{}, provider.NewProviderError(provider.FailMalformedResponse, false, 0, httpStatus, "binary noul answer missing probability value")
		}
		p := *wa.Noul
		dist, boolVal, tied, err := provider.BinaryDistribution(p)
		if err != nil {
			return provider.Answer{}, withHTTPStatus(err, httpStatus)
		}
		nativeMetrics, conf, err := decodeConfidenceMetric(wa.Confidence, httpStatus)
		if err != nil {
			return provider.Answer{}, err
		}
		ansNotes := []string{provider.NoteClamped001To099}
		expectedConf := math.Max(p, 1.0-p)
		if math.Abs(conf-expectedConf) > confidenceFormulaTolerance {
			ansNotes = append(ansNotes, provider.NoteConfidenceFormulaDeviation)
		}
		return provider.Answer{
			QuestionID:    q.ID,
			Primitive:     spec.PrimitiveBinary,
			Status:        provider.AnswerStatusAnswered,
			Tied:          tied,
			Bool:          boolVal,
			Distribution:  dist,
			NativeMetrics: nativeMetrics,
			Notes:         ansNotes,
		}, nil

	case spec.PrimitiveChoice:
		if wa.Type != wireTypeChoice {
			return provider.Answer{}, provider.NewProviderError(provider.FailMalformedResponse, false, 0, httpStatus, "choice answer type must be \"choice\"")
		}
		if wa.Probabilities == nil {
			return provider.Answer{}, provider.NewProviderError(provider.FailMalformedResponse, false, 0, httpStatus, "choice answer missing probabilities map")
		}

		declaredIDs := make([]string, len(q.Options))
		for i, opt := range q.Options {
			declaredIDs[i] = opt.ID
		}

		distCopy := make(map[string]float64, len(wa.Probabilities))
		var pMax float64
		for k, pPtr := range wa.Probabilities {
			if pPtr == nil {
				return provider.Answer{}, provider.NewProviderError(provider.FailMalformedResponse, false, 0, httpStatus, "choice probability value must not be null")
			}
			p := *pPtr
			distCopy[k] = p
			if p > pMax {
				pMax = p
			}
		}
		if err := provider.ValidateDistribution(distCopy, declaredIDs); err != nil {
			return provider.Answer{}, withHTTPStatus(err, httpStatus)
		}

		if wa.Choice == nil {
			return provider.Answer{}, provider.NewProviderError(provider.FailMalformedResponse, false, 0, httpStatus, "choice field must not be null")
		}
		argmax, tied := provider.ArgmaxChoice(distCopy)
		var choicePtr *string
		if !tied {
			if *wa.Choice != argmax {
				return provider.Answer{}, provider.NewProviderError(provider.FailMalformedResponse, false, 0, httpStatus, "choice field does not match argmax of probabilities")
			}
			cCopy := *wa.Choice
			choicePtr = &cCopy
		} else {
			chosenProb, exists := distCopy[*wa.Choice]
			if !exists || pMax-chosenProb > provider.TieTolerance {
				return provider.Answer{}, provider.NewProviderError(provider.FailMalformedResponse, false, 0, httpStatus, "choice field on tie is not one of the tied top options")
			}
		}

		nativeMetrics, conf, err := decodeConfidenceMetric(wa.Confidence, httpStatus)
		if err != nil {
			return provider.Answer{}, err
		}
		ansNotes := []string{provider.NoteOptionsSortedByProvider}
		if math.Abs(conf-pMax) > confidenceFormulaTolerance {
			ansNotes = append(ansNotes, provider.NoteConfidenceFormulaDeviation)
		}

		return provider.Answer{
			QuestionID:    q.ID,
			Primitive:     spec.PrimitiveChoice,
			Status:        provider.AnswerStatusAnswered,
			Tied:          tied,
			Choice:        choicePtr,
			Distribution:  distCopy,
			NativeMetrics: nativeMetrics,
			Notes:         ansNotes,
		}, nil

	default:
		return provider.Answer{}, provider.NewProviderError(provider.FailUnsupported, false, 0, httpStatus, "question primitive is not supported")
	}
}

func decodeConfidenceMetric(confPtr *float64, httpStatus int) (map[string]provider.Metric, float64, error) {
	if confPtr == nil {
		return nil, 0, provider.NewProviderError(provider.FailMalformedResponse, false, 0, httpStatus, "answer missing required confidence field")
	}
	conf := *confPtr
	if math.IsNaN(conf) || math.IsInf(conf, 0) || conf < 0 || conf > 1 {
		return nil, 0, provider.NewProviderError(provider.FailMalformedResponse, false, 0, httpStatus, "answer confidence is non-finite or outside [0, 1]")
	}
	return map[string]provider.Metric{
		confidenceMetricDecl.Name: {
			Value:  conf,
			Unit:   confidenceMetricDecl.Unit,
			Min:    confidenceMetricDecl.Min,
			Max:    confidenceMetricDecl.Max,
			Source: confidenceMetricDecl.Source,
		},
	}, conf, nil
}

func withHTTPStatus(err error, httpStatus int) error {
	pe, ok := provider.AsProviderError(err)
	if !ok {
		return provider.NewProviderError(provider.FailMalformedResponse, false, 0, httpStatus, "malformed provider distribution")
	}
	pe.HTTPStatus = httpStatus
	return pe
}

func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	if len(out) > 0 && out[len(out)-1] == '\n' {
		out = out[:len(out)-1]
	}
	return out, nil
}

func checkNoDuplicateJSONKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := walkNoDuplicateKeys(dec, 0); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("unexpected trailing JSON token")
	}
	return nil
}

func walkNoDuplicateKeys(dec *json.Decoder, depth int) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		nextDepth := depth + 1
		if nextDepth > maxJSONNestingDepth {
			return fmt.Errorf("JSON nesting depth exceeds maximum %d", maxJSONNestingDepth)
		}
		seen := make(map[string]struct{})
		for dec.More() {
			kTok, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := kTok.(string)
			if !ok {
				return fmt.Errorf("expected string key")
			}
			if _, dup := seen[key]; dup {
				return fmt.Errorf("duplicate JSON key")
			}
			seen[key] = struct{}{}
			if err := walkNoDuplicateKeys(dec, nextDepth); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil {
			return err
		}
	case '[':
		nextDepth := depth + 1
		if nextDepth > maxJSONNestingDepth {
			return fmt.Errorf("JSON nesting depth exceeds maximum %d", maxJSONNestingDepth)
		}
		for dec.More() {
			if err := walkNoDuplicateKeys(dec, nextDepth); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil {
			return err
		}
	}
	return nil
}
