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

package jevwire

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
)

const (
	wireTypeNoul   = "noul"
	wireTypeChoice = "choice"

	confidenceRangeTolerance    = 1e-6
	confidenceFormulaTolerance  = 1e-3
	maxSanitizedRequestIDLength = 128
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
	True  string `json:"true"`
	False string `json:"false"`
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

type wireSystemOneRequest struct {
	Model     string             `json:"model"`
	State     orderedStringMap   `json:"state"`
	Questions orderedQuestionMap `json:"questions"`
}

// EncodeRequest serializes req into a Jev-wire SystemOneRequest JSON payload
// (UTF-8 without BOM, no HTML escaping, preserving declared field, question,
// and option order) and returns the encoded bytes alongside the ordered
// questions included in this request.
func EncodeRequest(model string, req provider.ModelRequest) ([]byte, []spec.Question, error) {
	trimmedModel := strings.TrimSpace(model)
	if trimmedModel == "" {
		return nil, nil, provider.NewProviderError(provider.FailInvalidRequest, false, 0, 0, "instance model is required on every request")
	}
	if req.Rule.Form == spec.FormNative {
		return nil, nil, provider.NewProviderError(provider.FailUnsupported, false, 0, 0, "native specs are not supported in P1")
	}

	selectedQuestions, err := selectRequestQuestions(req.Rule, req.Questions)
	if err != nil {
		return nil, nil, err
	}

	state := make(orderedStringMap, 0, len(req.Input))
	seenInputs := make(map[string]struct{}, len(req.Input))
	for _, f := range req.Input {
		if f.Name == "" {
			return nil, nil, provider.NewProviderError(provider.FailInvalidRequest, false, 0, 0, "input field name must not be empty")
		}
		if _, dup := seenInputs[f.Name]; dup {
			return nil, nil, provider.NewProviderError(provider.FailInvalidRequest, false, 0, 0, "duplicate input field name in request")
		}
		seenInputs[f.Name] = struct{}{}
		state = append(state, stringKV{Key: f.Name, Value: f.Value})
	}

	wireQs := make(orderedQuestionMap, 0, len(selectedQuestions))
	for _, q := range selectedQuestions {
		wq, qErr := encodeQuestion(q)
		if qErr != nil {
			return nil, nil, qErr
		}
		wireQs = append(wireQs, questionKV{ID: q.ID, Question: wq})
	}

	payload := wireSystemOneRequest{
		Model:     trimmedModel,
		State:     state,
		Questions: wireQs,
	}
	encoded, err := marshalNoEscape(payload)
	if err != nil {
		return nil, nil, provider.NewProviderError(provider.FailInvalidRequest, false, 0, 0, "failed to marshal Jev request JSON")
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
				True:  q.Criteria.True,
				False: q.Criteria.False,
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
		return wireQuestion{}, provider.NewProviderError(provider.FailUnsupported, false, 0, 0, "question primitive is not supported by jevwire in P1")
	}
}

type wireSystemOneResponse struct {
	Model   *string                    `json:"model"`
	Answers map[string]json.RawMessage `json:"answers"`
	Usage   *wireUsage                 `json:"usage"`
}

type wireUsage struct {
	InputTokens  *int  `json:"input_tokens"`
	OutputTokens *int  `json:"output_tokens"`
	Truncated    *bool `json:"truncated,omitempty"`
}

type wireAnswer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul"`
	Choice        *string            `json:"choice"`
	Confidence    *float64           `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

// DecodeResponse validates and decodes a 2xx POST /v1/systemone response body
// according to profile and the requested questions (provider-design §6.3, §7.4).
func DecodeResponse(profile Profile, questions []spec.Question, httpStatus int, requestID string, body []byte) (provider.ModelResult, error) {
	if err := checkNoDuplicateJSONKeys(body); err != nil {
		return provider.ModelResult{}, provider.NewProviderError(provider.FailMalformedResponse, false, 0, httpStatus, "malformed or duplicate-key JSON in provider response")
	}

	var raw wireSystemOneResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return provider.ModelResult{}, provider.NewProviderError(provider.FailMalformedResponse, false, 0, httpStatus, "failed to decode provider JSON response")
	}

	if raw.Model == nil || strings.TrimSpace(*raw.Model) == "" {
		return provider.ModelResult{}, provider.NewProviderError(provider.FailMalformedResponse, false, 0, httpStatus, "provider response missing required model field")
	}
	if raw.Answers == nil {
		return provider.ModelResult{}, provider.NewProviderError(provider.FailMalformedResponse, false, 0, httpStatus, "provider response missing required answers object")
	}
	if raw.Usage == nil || raw.Usage.InputTokens == nil || raw.Usage.OutputTokens == nil {
		return provider.ModelResult{}, provider.NewProviderError(provider.FailMalformedResponse, false, 0, httpStatus, "provider response missing required usage token counts")
	}
	if *raw.Usage.InputTokens < 0 || *raw.Usage.OutputTokens < 0 {
		return provider.ModelResult{}, provider.NewProviderError(provider.FailMalformedResponse, false, 0, httpStatus, "provider response reported negative token counts")
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
		decoded, err := decodeQuestionAnswer(profile, q, httpStatus, rawAns)
		if err != nil {
			return provider.ModelResult{}, err
		}
		answers[q.ID] = decoded
	}

	resolvedModel := *raw.Model
	var resultNotes []string
	for _, alias := range profile.KnownAliases {
		if resolvedModel == alias {
			resultNotes = append(resultNotes, provider.NoteModelAliasReported)
			break
		}
	}

	usage := provider.Usage{
		InputTokens:  *raw.Usage.InputTokens,
		OutputTokens: *raw.Usage.OutputTokens,
	}
	if profile.AllowReportedTruncation && raw.Usage.Truncated != nil {
		tCopy := *raw.Usage.Truncated
		usage.Truncated = &tCopy
	}

	return provider.ModelResult{
		Answers:           answers,
		ResolvedModel:     resolvedModel,
		ProviderRequestID: sanitizeRequestID(requestID),
		HTTPStatus:        httpStatus,
		Usage:             usage,
		Notes:             resultNotes,
	}, nil
}

func decodeQuestionAnswer(profile Profile, q spec.Question, httpStatus int, rawAns json.RawMessage) (provider.Answer, error) {
	if len(bytes.TrimSpace(rawAns)) == 0 || bytes.Equal(bytes.TrimSpace(rawAns), []byte("null")) {
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
		dist, boolVal, tied, err := provider.BinaryDistribution(*wa.Noul)
		if err != nil {
			return provider.Answer{}, withHTTPStatus(err, httpStatus)
		}
		return provider.Answer{
			QuestionID:   q.ID,
			Primitive:    spec.PrimitiveBinary,
			Status:       provider.AnswerStatusAnswered,
			Tied:         tied,
			Bool:         boolVal,
			Distribution: dist,
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
		if err := provider.ValidateDistribution(wa.Probabilities, declaredIDs); err != nil {
			return provider.Answer{}, withHTTPStatus(err, httpStatus)
		}

		distCopy := make(map[string]float64, len(wa.Probabilities))
		var pMax float64
		for k, p := range wa.Probabilities {
			distCopy[k] = p
			if p > pMax {
				pMax = p
			}
		}

		argmax, tied := provider.ArgmaxChoice(distCopy)
		var choicePtr *string
		if !tied {
			if wa.Choice == nil || *wa.Choice != argmax {
				return provider.Answer{}, provider.NewProviderError(provider.FailMalformedResponse, false, 0, httpStatus, "choice field does not match argmax of probabilities")
			}
			cCopy := *wa.Choice
			choicePtr = &cCopy
		} else {
			if wa.Choice == nil {
				if !profile.ChoiceNullOnTie {
					return provider.Answer{}, provider.NewProviderError(provider.FailMalformedResponse, false, 0, httpStatus, "choice field must not be null for this profile")
				}
			} else {
				chosenProb, exists := distCopy[*wa.Choice]
				if !exists || pMax-chosenProb > provider.TieTolerance {
					return provider.Answer{}, provider.NewProviderError(provider.FailMalformedResponse, false, 0, httpStatus, "choice field on tie is not one of the tied top options")
				}
			}
		}

		var (
			nativeMetrics map[string]provider.Metric
			ansNotes      []string
		)
		if profile.RequireChoiceConfidence && wa.Confidence == nil {
			return provider.Answer{}, provider.NewProviderError(provider.FailMalformedResponse, false, 0, httpStatus, "choice answer missing required confidence field")
		}
		if wa.Confidence != nil && profile.ConfidenceMetric != nil {
			conf := *wa.Confidence
			if math.IsNaN(conf) || math.IsInf(conf, 0) || conf < -confidenceRangeTolerance || conf > 1.0+confidenceRangeTolerance {
				return provider.Answer{}, provider.NewProviderError(provider.FailMalformedResponse, false, 0, httpStatus, "choice confidence is non-finite or outside [0, 1]")
			}
			nativeMetrics = map[string]provider.Metric{
				profile.ConfidenceMetric.Name: {
					Value:  conf,
					Unit:   profile.ConfidenceMetric.Unit,
					Min:    profile.ConfidenceMetric.Min,
					Max:    profile.ConfidenceMetric.Max,
					Source: profile.ConfidenceMetric.Source,
				},
			}
			if profile.CheckConfidenceFormula && len(declaredIDs) >= 2 {
				n := float64(len(declaredIDs))
				expected := (n*pMax - 1.0) / (n - 1.0)
				if math.Abs(conf-expected) > confidenceFormulaTolerance {
					ansNotes = append(ansNotes, NoteConfidenceFormulaDeviation)
				}
			}
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

type wireModelMetadataList struct {
	Models *[]wireModelMetadata `json:"models"`
}

type wireModelMetadata struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	ReleaseDate *string `json:"release_date"`
}

// DecodeModelsResponse validates and decodes a 2xx GET /v1/models response body
// matching TypeSafe OpenAPI 0.2.0 ModelMetadataList -> []ModelMetadata.
func DecodeModelsResponse(httpStatus int, body []byte) ([]string, error) {
	if err := checkNoDuplicateJSONKeys(body); err != nil {
		return nil, provider.NewProviderError(provider.FailMalformedResponse, false, 0, httpStatus, "malformed or duplicate-key JSON in models response")
	}
	var parsed wireModelMetadataList
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.Models == nil {
		return nil, provider.NewProviderError(provider.FailMalformedResponse, false, 0, httpStatus, "invalid GET /v1/models ModelMetadataList payload")
	}
	out := make([]string, 0, len(*parsed.Models))
	for _, m := range *parsed.Models {
		if m.Name == nil || strings.TrimSpace(*m.Name) == "" || m.Description == nil || m.ReleaseDate == nil || strings.TrimSpace(*m.ReleaseDate) == "" {
			return nil, provider.NewProviderError(provider.FailMalformedResponse, false, 0, httpStatus, "model metadata entry missing required name, description, or release_date")
		}
		out = append(out, *m.Name)
	}
	return out, nil
}

func withHTTPStatus(err error, httpStatus int) error {
	pe, ok := provider.AsProviderError(err)
	if !ok {
		return provider.NewProviderError(provider.FailMalformedResponse, false, 0, httpStatus, "malformed provider distribution")
	}
	pe.HTTPStatus = httpStatus
	return pe
}

func sanitizeRequestID(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if len(s) > maxSanitizedRequestIDLength {
		s = s[:maxSanitizedRequestIDLength]
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] >= 0x7f {
			return ""
		}
	}
	return s
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

const maxJSONNestingDepth = 32

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
