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
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/gitref"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/labels"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/triggertemplate"
)

// TriggerRenderUsedVar describes one placeholder path expanded while rendering
// a trigger slot. Values are never attached to this DTO.
type TriggerRenderUsedVar struct {
	Path  string `json:"path"`
	Class string `json:"class"`
}

// TriggerRenderSlotResult holds the rendered text and used variables for one slot.
type TriggerRenderSlotResult struct {
	Rendered   string                 `json:"rendered"`
	Used       []TriggerRenderUsedVar `json:"used"`
	Templating string                 `json:"templating,omitempty"`
}

// TriggerRenderSlots holds rendered slot outputs keyed by slot name.
type TriggerRenderSlots struct {
	Task         *TriggerRenderSlotResult           `json:"task,omitempty"`
	Name         *TriggerRenderSlotResult           `json:"name,omitempty"`
	Branch       *TriggerRenderSlotResult           `json:"branch,omitempty"`
	Labels       map[string]TriggerRenderSlotResult `json:"labels,omitempty"`
	ReuseMessage *TriggerRenderSlotResult           `json:"reuseMessage,omitempty"`
	Message      *TriggerRenderSlotResult           `json:"message,omitempty"`
	Body         *TriggerRenderSlotResult           `json:"body,omitempty"`
}

// TriggerRenderError describes one content-free compile or render failure for a slot.
type TriggerRenderError struct {
	Slot    string `json:"slot"`
	Path    string `json:"path,omitempty"`
	Kind    string `json:"kind"`
	Message string `json:"message,omitempty"`
}

// TriggerRenderResponse is the API representation of
// POST /api/v1/projects/{projectId}/triggers/{id}/render?eventId=<id> (Stage 2 §5.8).
// Expanded event values appear only in this response and are never logged.
type TriggerRenderResponse struct {
	Slots  TriggerRenderSlots   `json:"slots"`
	Errors []TriggerRenderError `json:"errors"`
}

// renderTrigger handles POST /api/v1/projects/{projectId}/triggers/{id}/render?eventId=<id>.
// It requires trigger.read on the trigger and read access on the event under the
// caller's own identity, exercises no recorded authority, creates nothing, and
// never logs event or rendered values (Stage 2 §5.8, §9.2).
func (s *Server) renderTrigger(w http.ResponseWriter, r *http.Request, projectID, triggerID string) {
	ctx := r.Context()

	tr, ok := s.loadTriggerForProject(w, r, projectID, triggerID)
	if !ok {
		return
	}
	if !s.authorizeTriggerRow(w, r, projectID, ActionRead, triggerRowOf(tr)) {
		return
	}

	eventID := strings.TrimSpace(r.URL.Query().Get("eventId"))
	if eventID == "" {
		ValidationError(w, "eventId query parameter is required", nil)
		return
	}

	ev, err := s.store.GetTriggerEvent(ctx, eventID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidInput) {
			NotFound(w, "Trigger event")
			return
		}
		slog.ErrorContext(ctx, "trigger render: failed to load trigger event",
			"project_id", projectID, "trigger_id", triggerID, "event_id", eventID, "error", err)
		writeErrorFromErr(w, err, "")
		return
	}
	if ev == nil || ev.ProjectID != projectID {
		NotFound(w, "Trigger event")
		return
	}
	if !s.authorizeTriggerEventRow(w, r, projectID, ActionRead, ev) {
		return
	}

	var project *store.Project
	projectName := ""
	if p, pErr := s.store.GetProject(ctx, projectID); pErr == nil && p != nil {
		project = p
		projectName = p.Name
	}

	sourceName := ""
	signatureMode := ""
	if ev.SourceKind == store.TriggerSourceKindWebhook && ev.SourceID != "" {
		if ep, epErr := s.store.GetWebhookEndpoint(ctx, ev.SourceID); epErr == nil && ep != nil && ep.ProjectID == projectID {
			sourceName = ep.Name
			signatureMode = ep.SignatureMode
		}
	}
	if sourceName == "" && ev.Attributes != nil {
		sourceName = ev.Attributes["endpoint_name"]
	}
	if signatureMode == "" && ev.Attributes != nil {
		signatureMode = ev.Attributes["signature_mode"]
	}

	fireReq := hubfire.FireRequest{
		ProjectID: projectID,
		Authority: hubfire.RecordedAuthority{
			DefinitionKind: hubfire.DefinitionKindTrigger,
			DefinitionID:   tr.ID,
		},
		Event: &hubfire.FireEvent{
			ID:         ev.ID,
			SourceKind: ev.SourceKind,
			SourceID:   ev.SourceID,
			Type:       ev.Type,
			Subject:    ev.Subject,
			OccurredAt: ev.OccurredAt,
			ReceivedAt: ev.ReceivedAt,
			Attributes: ev.Attributes,
			Lists:      ev.Lists,
			Payload:    []byte(ev.Payload),
			Trigger: &hubfire.FireTriggerRef{
				ID:          tr.ID,
				Name:        tr.Name,
				SubjectMode: tr.SubjectMode,
			},
		},
	}
	vars := buildDispatchTemplateVars(fireReq, &dispatchAgentAuthorized{
		projectName:   projectName,
		sourceName:    sourceName,
		signatureMode: signatureMode,
	})

	frameCtx := triggertemplate.FrameContext{
		EndpointName:  sourceName,
		SignatureMode: signatureMode,
		EventID:       ev.ID,
	}
	if ev.SourceKind == store.TriggerSourceKindGitHub {
		frameCtx.Source = "GitHub App"
	}
	frameOpt := triggertemplate.WithFrame(frameCtx)

	resp := TriggerRenderResponse{
		Errors: []TriggerRenderError{},
	}

	switch tr.ActionType {
	case store.TriggerActionDispatchAgent:
		var p DispatchAgentEventPayload
		if err := json.Unmarshal([]byte(tr.ActionPayload), &p); err != nil {
			resp.Errors = append(resp.Errors, TriggerRenderError{
				Slot:    "action",
				Kind:    "invalid_payload",
				Message: "stored dispatch_agent action payload could not be decoded",
			})
			writeJSON(w, http.StatusOK, resp)
			return
		}

		if res, rErr := renderSlotPreview(p.Task, triggertemplate.SlotTask, vars, p.UntrustedVars, frameOpt); rErr != nil {
			resp.Errors = append(resp.Errors, toTriggerRenderError(triggertemplate.SlotTask, rErr))
		} else {
			resp.Slots.Task = &res
		}

		if p.AgentName != "" {
			if res, rErr := renderSlotPreview(p.AgentName, triggertemplate.SlotName, vars, p.UntrustedVars); rErr != nil {
				resp.Errors = append(resp.Errors, toTriggerRenderError(triggertemplate.SlotName, rErr))
			} else {
				resp.Slots.Name = &res
			}
		}

		if p.Branch != "" {
			if res, rErr := renderSlotPreview(p.Branch, triggertemplate.SlotBranch, vars, p.UntrustedVars); rErr != nil {
				resp.Errors = append(resp.Errors, toTriggerRenderError(triggertemplate.SlotBranch, rErr))
			} else {
				if trimmed, cut := strings.CutPrefix(res.Rendered, "refs/heads/"); cut {
					if err := gitref.ValidBranch(trimmed); err != nil {
						resp.Errors = append(resp.Errors, TriggerRenderError{
							Slot:    triggertemplate.SlotBranch,
							Kind:    string(triggertemplate.KindValueRejected),
							Message: "rendered branch failed validation after stripping refs/heads/ prefix",
						})
					} else {
						res.Rendered = trimmed
						resp.Slots.Branch = &res
					}
				} else {
					resp.Slots.Branch = &res
				}
			}
		}

		if len(p.Labels) > 0 {
			keys := make([]string, 0, len(p.Labels))
			for k := range p.Labels {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			renderedLabels := make(map[string]TriggerRenderSlotResult, len(keys))
			merged := make(map[string]string, len(keys)+hubAppliedTriggerLabelCount)
			allLabelsOK := true
			for _, k := range keys {
				res, rErr := renderSlotPreview(p.Labels[k], triggertemplate.SlotLabel, vars, p.UntrustedVars)
				if rErr != nil {
					allLabelsOK = false
					resp.Errors = append(resp.Errors, toTriggerRenderError(triggertemplate.SlotLabel+"."+k, rErr))
					continue
				}
				renderedLabels[k] = res
				merged[k] = res.Rendered
			}
			if len(renderedLabels) > 0 {
				resp.Slots.Labels = renderedLabels
			}
			if allLabelsOK {
				merged[triggerLabelKeyTrigger] = tr.ID
				merged[triggerLabelKeyTriggerEvent] = ev.ID
				merged[triggerLabelKeyTriggerSource] = ev.SourceKind
				if err := labels.Validate(merged); err != nil {
					resp.Errors = append(resp.Errors, TriggerRenderError{
						Slot:    triggertemplate.SlotLabel,
						Kind:    string(triggertemplate.KindValueRejected),
						Message: "rendered labels failed validation",
					})
				}
			}
		}

		if p.ReuseMessage != "" {
			if res, rErr := renderSlotPreview(p.ReuseMessage, triggertemplate.SlotMessage, vars, p.UntrustedVars); rErr != nil {
				resp.Errors = append(resp.Errors, toTriggerRenderError("reuseMessage", rErr))
			} else {
				resp.Slots.ReuseMessage = &res
			}
		}

	case store.TriggerActionMessage:
		var p MessageEventPayload
		if err := json.Unmarshal([]byte(tr.ActionPayload), &p); err != nil {
			resp.Errors = append(resp.Errors, TriggerRenderError{
				Slot:    "action",
				Kind:    "invalid_payload",
				Message: "stored message action payload could not be decoded",
			})
			writeJSON(w, http.StatusOK, resp)
			return
		}
		// Message action fires deliver Message literally until Stage 2 §5.7 (P2b)
		// lands message templating; mirror fire-path delivery verbatim here.
		resp.Slots.Message = &TriggerRenderSlotResult{
			Rendered:   p.Message,
			Used:       []TriggerRenderUsedVar{},
			Templating: "not_supported_until_P2b",
		}

	case store.TriggerActionGitHubComment:
		var p GitHubCommentActionPayload
		if err := json.Unmarshal([]byte(tr.ActionPayload), &p); err != nil {
			resp.Errors = append(resp.Errors, TriggerRenderError{
				Slot:    "action",
				Kind:    "invalid_payload",
				Message: "stored github.comment action payload could not be decoded",
			})
			writeJSON(w, http.StatusOK, resp)
			return
		}
		commentVars := buildGitHubCommentTemplateVars(fireReq.Event, project)
		if res, rErr := renderSlotPreview(p.Body, TriggerTemplateSlotGitHubCommentBody, commentVars, p.UntrustedVars); rErr != nil {
			resp.Errors = append(resp.Errors, toTriggerRenderError(TriggerTemplateSlotGitHubCommentBody, rErr))
		} else {
			resp.Slots.Body = &res
		}

	default:
		resp.Errors = append(resp.Errors, TriggerRenderError{
			Slot:    "action",
			Kind:    "unsupported_action",
			Message: "trigger action type does not support template rendering",
		})
	}

	writeJSON(w, http.StatusOK, resp)
}

func renderSlotPreview(raw, slot string, vars triggertemplate.Vars, allowed []string, opts ...triggertemplate.RenderOption) (TriggerRenderSlotResult, error) {
	tmpl, err := triggertemplate.Parse(raw, slot)
	if err != nil {
		return TriggerRenderSlotResult{}, err
	}
	rendered, report, err := tmpl.Render(vars, allowed, opts...)
	if err != nil {
		return TriggerRenderSlotResult{}, err
	}
	used := make([]TriggerRenderUsedVar, len(report.Used))
	for i, u := range report.Used {
		used[i] = TriggerRenderUsedVar{
			Path:  u.Path,
			Class: string(u.Class),
		}
	}
	return TriggerRenderSlotResult{
		Rendered: rendered,
		Used:     used,
	}, nil
}

func toTriggerRenderError(slot string, err error) TriggerRenderError {
	var re *triggertemplate.RenderError
	if errors.As(err, &re) {
		return TriggerRenderError{
			Slot:    slot,
			Path:    re.Path,
			Kind:    string(re.Kind),
			Message: re.Reason,
		}
	}
	var ue *triggertemplate.UntrustedVarError
	if errors.As(err, &ue) {
		return TriggerRenderError{
			Slot:    slot,
			Path:    ue.Path,
			Kind:    "untrusted_var_not_allowed",
			Message: ue.Error(),
		}
	}
	var pe *triggertemplate.ParseError
	if errors.As(err, &pe) {
		return TriggerRenderError{
			Slot:    slot,
			Kind:    "invalid_template",
			Message: pe.Message,
		}
	}
	return TriggerRenderError{
		Slot:    slot,
		Kind:    string(triggertemplate.KindValueRejected),
		Message: "template render failed",
	}
}
