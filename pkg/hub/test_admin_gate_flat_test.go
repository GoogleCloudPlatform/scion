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

//go:build !no_sqlite

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	tiaFlatBrokerA = "0a0a0a0a-0000-4000-8000-0000000000fa"
	tiaFlatBrokerB = "0a0a0a0a-0000-4000-8000-0000000000fb"
)

// While the test admin tier is on, activated co-located flat Runtime Broker
// instances are admitted like the embedded broker (several share the
// process, so none is the single embedded broker ID): broker authentication
// admits each, the startup broker set check passes with their rows online,
// and a remote broker is still refused. An instance that stops being
// embedded is refused again.
func TestTestAdminBroker_ColocatedFlatInstancesAdmitted(t *testing.T) {
	buf := tiaCaptureAudit(t)
	srv, s := testServer(t)
	ctx := context.Background()
	tiaBroker(t, s, tiaFlatBrokerA, store.BrokerStatusOnline, nil)
	tiaBroker(t, s, tiaFlatBrokerB, store.BrokerStatusOnline, nil)

	srv.ExpectEmbeddedBroker()
	srv.ExpectEmbeddedFlatInstances([]string{"k8s-a", "docker-b"})
	srv.mu.Lock()
	srv.recordEmbeddedFlatActivatedLocked("k8s-a", tiaFlatBrokerA)
	srv.recordEmbeddedFlatActivatedLocked("docker-b", tiaFlatBrokerB)
	srv.mu.Unlock()
	require.Empty(t, srv.embeddedBrokerSnapshot().id, "precondition: two instances, so no single embedded broker ID")

	enableTestAdminGateForTest(srv)
	for _, id := range []string{tiaFlatBrokerA, tiaFlatBrokerB} {
		assert.NoError(t, srv.testAdminBrokerAdmission(ctx, id), "co-located flat instance %s", id)
	}
	assert.ErrorIs(t, srv.testAdminBrokerAdmission(ctx, tiaRemoteBrokerID), ErrTestAdminRemoteBrokerRefused)
	assert.NoError(t, srv.VerifyTestAdminBrokerSet(ctx), "both flat instance rows online")
	line := tiaBrokerSetAuditLine(t, buf.String())
	assert.Equal(t, []interface{}{tiaFlatBrokerA, tiaFlatBrokerB}, line["flat_broker_ids"], "the audit line names the admitted flat instances")
	assert.Equal(t, "", line["embedded_broker_id"])

	tiaBroker(t, s, tiaRemoteOnlineID, store.BrokerStatusOnline, nil)
	err := srv.VerifyTestAdminBrokerSet(ctx)
	require.ErrorIs(t, err, ErrTestAdminRemoteBrokerActive)
	assert.Contains(t, err.Error(), tiaRemoteOnlineID)
	require.NoError(t, s.DeleteRuntimeBroker(ctx, tiaRemoteOnlineID))

	srv.EmbeddedFlatInstanceFailed("docker-b", errors.New("stopped serving"))
	assert.ErrorIs(t, srv.testAdminBrokerAdmission(ctx, tiaFlatBrokerB), ErrTestAdminRemoteBrokerRefused,
		"an instance that is no longer embedded")
	assert.NoError(t, srv.testAdminBrokerAdmission(ctx, tiaFlatBrokerA))
}

// tiaBrokerSetAuditLine returns the last test admin broker-set audit line
// in out, decoded.
func tiaBrokerSetAuditLine(t *testing.T, out string) map[string]interface{} {
	t.Helper()
	var last map[string]interface{}
	for _, l := range strings.Split(out, "\n") {
		if !strings.Contains(l, testAdminAuditEventBrokerSet) {
			continue
		}
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(l[strings.Index(l, "{"):]), &m); err == nil {
			last = m
		}
	}
	require.NotNil(t, last, "no %s audit line in: %s", testAdminAuditEventBrokerSet, out)
	return last
}
