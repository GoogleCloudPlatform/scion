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

package brokerhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
)

// TestHostHealth_ReasonCodesOnly: the unauthenticated health responses name
// each instance's state and a stable reason code, never the refusal text
// (which can carry local paths and endpoints).
func TestHostHealth_ReasonCodesOnly(t *testing.T) {
	const secretDetail = "/etc/scion/private/cluster-b.kubeconfig at https://10.1.2.3:6443"
	f := newFixture(t)
	f.probeErr["docker-a"] = fmt.Errorf("%w: reading Namespace kube-system failed: %s", brokeridentity.ErrExecutionScopeUnidentified, secretDetail)
	f.activator.refuse = map[string]error{"docker-b": errors.New("registration failed: " + secretDetail)}
	h, err := New(f.config(t, dockerInstance("docker-a", "a"), dockerInstance("docker-b", "b"), dockerInstance("docker-c", "c")))
	require.NoError(t, err)
	require.NoError(t, h.Prepare(context.Background()))

	for _, path := range []string{"/healthz", "/readyz"} {
		rec := get(t, h.Handler(), path)
		body := rec.Body.String()
		assert.NotContains(t, body, secretDetail, "%s carries refusal text", path)
		assert.NotContains(t, body, `"error"`)
		var resp healthResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), body)
		reasons := map[string]string{}
		for _, st := range resp.Instances {
			reasons[st.Key] = st.Reason
		}
		assert.Equal(t, "scope_unidentified", reasons["docker-a"])
		assert.Equal(t, "refused", reasons["docker-b"])
		assert.Equal(t, "", reasons["docker-c"])
	}
	assert.Equal(t, http.StatusServiceUnavailable, get(t, h.Handler(), "/readyz").Code)

	// In-process callers still get the full text.
	st := statusByKey(h)
	assert.Contains(t, st["docker-a"].Error, secretDetail)
}

func TestReasonCode(t *testing.T) {
	for want, err := range map[string]error{
		api.ErrCodeRuntimeTargetAckMissing:        fmt.Errorf("x: %w", &brokeridentity.AckError{Code: api.ErrCodeRuntimeTargetAckMissing}),
		"scope_unidentified":                      fmt.Errorf("x: %w", brokeridentity.ErrExecutionScopeUnidentified),
		"scope_changed":                           fmt.Errorf("x: %w", brokeridentity.ErrExecutionScopeChanged),
		"identity_error":                          brokeridentity.ErrIdentityCorrupt,
		"runtime_build_failed":                    &runtimeBuildError{err: errors.New("x")},
		"not_serving":                             WithReason("not_serving", errors.New("x")),
		"experiment_disabled":                     WithReason("experiment_disabled", errors.New("x")),
		api.ErrCodeFlatRuntimeBrokerNotRegistered: codedErr(api.ErrCodeFlatRuntimeBrokerNotRegistered),
		"refused":                                 errors.New("anything else"),
	} {
		assert.Equal(t, want, ReasonCode(err), "%v", err)
	}
	assert.Equal(t, "", ReasonCode(nil))
}

type codedErr string

func (c codedErr) Error() string { return string(c) + ": detail" }
func (c codedErr) Code() string  { return string(c) }
