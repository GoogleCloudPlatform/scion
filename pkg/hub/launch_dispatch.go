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

// LaunchAccepted records that a broker accepted a create for asynchronous
// launch (design t1-async-create-v11.md §3.4): ID is the Hub launch ID the
// broker echoed and Owner is the broker process instance that claimed it.
type LaunchAccepted struct {
	ID    string `json:"id"`
	Owner string `json:"owner,omitempty"`
}

// CreateDispatchResult is the outcome of a launching create dispatch
// (DispatchAgentCreate, DispatchAgentCreateWithGather, DispatchFinalizeEnv).
// A nil result means the create completed synchronously, as before.
//
// At most one field is set:
//   - EnvReqs: the broker answered 202 and needs more env (env-gather).
//   - Launch: the broker accepted the create for asynchronous launch; the
//     agent row is in provisioning and the broker reports the outcome later.
//     Callers must not merge phase or message from the in-memory agent copy
//     in this case (the "accepted branch", design §3.5).
type CreateDispatchResult struct {
	EnvReqs *RemoteEnvRequirementsResponse `json:"envRequirements,omitempty"`
	Launch  *LaunchAccepted                `json:"launch,omitempty"`
}

// EnvRequirements returns r.EnvReqs, or nil when r is nil.
func (r *CreateDispatchResult) EnvRequirements() *RemoteEnvRequirementsResponse {
	if r == nil {
		return nil
	}
	return r.EnvReqs
}

// AcceptedLaunch returns r.Launch, or nil when r is nil.
func (r *CreateDispatchResult) AcceptedLaunch() *LaunchAccepted {
	if r == nil {
		return nil
	}
	return r.Launch
}

// envReqsResult wraps env requirements in a CreateDispatchResult, keeping a
// nil result for nil requirements.
func envReqsResult(envReqs *RemoteEnvRequirementsResponse) *CreateDispatchResult {
	if envReqs == nil {
		return nil
	}
	return &CreateDispatchResult{EnvReqs: envReqs}
}
