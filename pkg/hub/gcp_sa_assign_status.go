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
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Assign picker status (ptone/scion#3329 phase 4c). A project-scope list of
// GCP service accounts can annotate each account with whether it is mapped
// on one broker profile, from the same decision the dispatch refusal uses
// (profileSAMappingDecision). It is opt-in: without the query parameters
// below the list response is unchanged. Nothing is filtered: every account
// is returned with its state, and unknown is never folded into not_mapped.

// Query parameters that turn on the assign status. assignStatus=true asks
// for it with neither profile nor broker; either of those also asks for it.
const (
	assignStatusQueryParam  = "assignStatus"
	assignProfileQueryParam = "profile"
	assignBrokerQueryParam  = "broker"
)

// GCPServiceAccountAssignStatus is one account's mapping state on the chosen
// broker profile. Reason is set only when State is unknown. Message is
// human-readable and names no account or project: those are in the fields.
type GCPServiceAccountAssignStatus struct {
	State      string `json:"state"`
	Reason     string `json:"reason,omitempty"`
	Message    string `json:"message"`
	BrokerID   string `json:"brokerId,omitempty"`
	BrokerName string `json:"brokerName,omitempty"`
	Profile    string `json:"profile,omitempty"`
	Namespace  string `json:"namespace,omitempty"`
}

// assignStatusRequest is the parsed opt-in.
type assignStatusRequest struct {
	profile string
	broker  string
}

// parseAssignStatusRequest returns the request, or nil when the caller did
// not ask for the assign status.
func parseAssignStatusRequest(r *http.Request) *assignStatusRequest {
	q := r.URL.Query()
	req := &assignStatusRequest{
		profile: strings.TrimSpace(q.Get(assignProfileQueryParam)),
		broker:  strings.TrimSpace(q.Get(assignBrokerQueryParam)),
	}
	if q.Get(assignStatusQueryParam) != "true" && req.profile == "" && req.broker == "" {
		return nil
	}
	return req
}

// assignStatusBroker is the broker the picker's status is decided against.
// broker is nil when none is known; missingMessage then says why.
type assignStatusBroker struct {
	broker         *store.RuntimeBroker
	missingMessage string
}

// resolveAssignStatusBroker picks the broker the way agent creation does
// (resolveRuntimeBroker): the requested broker when it names a provider of
// the project by id, name or slug; otherwise the project's default broker
// when it is a provider; otherwise the only provider. It never fails the
// request: an unknown broker gives unknown/no_broker.
func (s *Server) resolveAssignStatusBroker(ctx context.Context, projectID, requested string) assignStatusBroker {
	providers, err := s.store.GetProjectProviders(ctx, projectID)
	if err != nil {
		slog.Debug("assign status: listing project providers failed", "project_id", projectID, "error", err)
		return assignStatusBroker{missingMessage: "The project's brokers could not be read."}
	}
	load := func(id string) assignStatusBroker {
		b, err := s.store.GetRuntimeBroker(ctx, id)
		if err != nil || b == nil {
			slog.Debug("assign status: broker unavailable", "broker", id, "error", err)
			return assignStatusBroker{missingMessage: "The broker could not be read."}
		}
		return assignStatusBroker{broker: b}
	}
	if requested != "" {
		for _, p := range providers {
			if p.BrokerID == requested || strings.EqualFold(p.BrokerName, requested) {
				return load(p.BrokerID)
			}
		}
		for _, p := range providers {
			b, err := s.store.GetRuntimeBroker(ctx, p.BrokerID)
			if err == nil && b != nil && b.Slug != "" && strings.EqualFold(b.Slug, requested) {
				return assignStatusBroker{broker: b}
			}
		}
		return assignStatusBroker{missingMessage: "The requested broker is not a provider for this project."}
	}
	project, err := s.store.GetProject(ctx, projectID)
	if err == nil && project != nil && project.DefaultRuntimeBrokerID != "" {
		for _, p := range providers {
			if p.BrokerID == project.DefaultRuntimeBrokerID {
				return load(p.BrokerID)
			}
		}
	}
	if len(providers) == 1 {
		return load(providers[0].BrokerID)
	}
	return assignStatusBroker{missingMessage: "No broker chosen, and the project has no default broker and more than one provider (or none)."}
}

// assignStatusMessages holds the fixed message for each state and reason.
var assignStatusMessages = map[string]string{
	SAMappingStateMapped: "Mapped: the broker's latest report for this profile lists a Kubernetes service account for it. " +
		"The Workload Identity IAM binding is not checked.",
	SAMappingStateNotMapped: "Not mapped: the broker's latest complete report for this profile lists no Kubernetes service account for it. " +
		"An agent assigned it on this profile is refused at start until a broker operator maps it.",
	SAMappingStateNotRequired: "No mapping needed: this profile's runtime is not Kubernetes.",

	SAMappingReasonNoProfile:           "Unknown: no profile chosen.",
	SAMappingReasonNoAccount:           "Unknown: the account has no email.",
	SAMappingReasonProfileNotOnBroker:  "Unknown: the broker has not registered a profile of this name.",
	SAMappingReasonRuntimeUnrecognized: "Unknown: the hub does not recognise this profile's runtime type, so it cannot tell whether a mapping is needed.",
	SAMappingReasonReportMissing:       "Unknown: the broker has not reported service account mappings for this profile.",
	SAMappingReasonReportIncomplete:    "Unknown: the broker's report for this profile is incomplete.",
	SAMappingReasonReportOldVersion:    "Unknown: the broker's report predates the current report version.",
	SAMappingReasonReportStale:         "Unknown: the broker's report for this profile is not recent.",
	SAMappingReasonAmbiguousMapping:    "Unknown: more than one Kubernetes service account is annotated for it; the broker decides.",
}

// assignStatusFor builds the status for one account.
func assignStatusFor(b assignStatusBroker, profile, email string, now time.Time) *GCPServiceAccountAssignStatus {
	d := profileSAMappingDecision(b.broker, profile, email, now)
	st := &GCPServiceAccountAssignStatus{
		State:      d.State,
		Reason:     d.Reason,
		BrokerName: d.Broker,
		Profile:    d.Profile,
		Namespace:  d.Namespace,
	}
	if b.broker != nil {
		st.BrokerID = b.broker.ID
	}
	switch {
	case d.Reason == SAMappingReasonNoBroker:
		st.Message = "Unknown: " + b.missingMessage
	case d.Reason != "":
		st.Message = assignStatusMessages[d.Reason]
	default:
		st.Message = assignStatusMessages[d.State]
	}
	return st
}

// annotateGCPSAAssignStatus sets the assign status on every listed account
// when req is non-nil. The broker and its profiles are read once.
func (s *Server) annotateGCPSAAssignStatus(ctx context.Context, items []GCPServiceAccountWithCapabilities, projectID string, req *assignStatusRequest) {
	if req == nil || len(items) == 0 {
		return
	}
	b := s.resolveAssignStatusBroker(ctx, projectID, req.broker)
	now := time.Now()
	for i := range items {
		items[i].AssignStatus = assignStatusFor(b, req.profile, items[i].Email, now)
	}
}
