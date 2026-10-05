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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// server.broker.instances (flat Runtime Broker instances) is a settings key
// the admin server-config editor never sends. A PUT that omits it must keep
// the stored value; only an explicit key changes or removes it
// (.design/flat-runtime-brokers-contract.md section 2).

// brokerInstancesInBody reports whether the raw PUT body carries
// server.broker.instances. When it does, the value is decoded strictly
// (unknown keys rejected) and validated; null decodes as nil (removal).
func brokerInstancesInBody(rawBody []byte) (present bool, instances []config.V1RuntimeBrokerInstanceConfig, err error) {
	var top map[string]json.RawMessage
	if json.Unmarshal(rawBody, &top) != nil {
		return false, nil, nil
	}
	var server map[string]json.RawMessage
	if json.Unmarshal(top["server"], &server) != nil || server == nil {
		return false, nil, nil
	}
	var broker map[string]json.RawMessage
	if json.Unmarshal(server["broker"], &broker) != nil || broker == nil {
		return false, nil, nil
	}
	raw, ok := broker["instances"]
	if !ok {
		return false, nil, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return true, nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&instances); err != nil {
		return true, nil, fmt.Errorf("invalid server.broker.instances: %w", err)
	}
	if errs := config.ValidateRuntimeBrokerInstances(instances); len(errs) > 0 {
		all := make([]error, 0, len(errs))
		for _, e := range errs {
			all = append(all, e)
		}
		return true, nil, errors.Join(all...)
	}
	return true, instances, nil
}

// storedBrokerInstances returns the raw server.broker.instances value of the
// settings map (nil when absent).
func storedBrokerInstances(raw map[string]interface{}) interface{} {
	server, _ := raw["server"].(map[string]interface{})
	broker, _ := server["broker"].(map[string]interface{})
	return broker["instances"]
}

// carryOverBrokerInstances runs after applySettingsUpdates. When the body did
// not carry server.broker.instances it restores the stored value (the
// one-level server merge replaces the whole broker map). When it did, an
// empty or null value removes the key and a list sets it.
func carryOverBrokerInstances(raw map[string]interface{}, stored interface{}, present bool, instances []config.V1RuntimeBrokerInstanceConfig) {
	server, _ := raw["server"].(map[string]interface{})
	broker, _ := server["broker"].(map[string]interface{})
	if broker == nil {
		if !present && stored != nil && server != nil {
			server["broker"] = map[string]interface{}{"instances": stored}
		}
		return
	}
	switch {
	case !present && stored != nil:
		broker["instances"] = stored
	case !present:
		// Nothing stored; nothing to restore.
	case len(instances) == 0:
		delete(broker, "instances")
	default:
		broker["instances"] = marshalToMap(instances)
	}
}

// nullPreservedBrokerPaths are settings keys a null on server or
// server.broker leaves in place in a workstation PUT. Unlike
// hubOwnedBrokerPaths they may be changed by an explicit key.
var nullPreservedBrokerPaths = [][]string{
	{"server", "broker", "instances"},
}
