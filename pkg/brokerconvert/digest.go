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

package brokerconvert

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// digestInput is what PlanDigest hashes. It deliberately holds no env
// value and no secret target: a hash of a short or guessable value could
// be tested offline by anyone who reads the journal. A changed env value
// is caught instead by the settings step, which writes only when the live
// list is empty or equal to the freshly computed full list.
type digestInput struct {
	// Instances are the publishable instances (config.PresentInstance).
	Instances []api.RuntimeTargetPresentation `json:"instances"`
	// EnvKeys are, per instance key and harness, the sorted env keys.
	EnvKeys    map[string]map[string][]string `json:"envKeys"`
	ProfileMap []ProfileMapping               `json:"profileMap"`
	Choices    []config.ConversionChoice      `json:"choices"`
}

// PlanDigest is the hex sha256 of the canonical JSON of p's publishable
// instances, the env keys of its instances (never their values), its
// profile map and choices. An apply resumes only with an equal digest.
func PlanDigest(p *Plan, choices []config.ConversionChoice) (string, error) {
	in := digestInput{
		Instances:  p.Instances,
		EnvKeys:    map[string]map[string][]string{},
		ProfileMap: p.ProfileMap,
		Choices:    choices,
	}
	if in.Instances == nil {
		in.Instances = []api.RuntimeTargetPresentation{}
	}
	if in.ProfileMap == nil {
		in.ProfileMap = []ProfileMapping{}
	}
	if in.Choices == nil {
		in.Choices = []config.ConversionChoice{}
	}
	for _, inst := range p.settingsInstances {
		if inst.AgentDefaults == nil {
			continue
		}
		for harness, o := range inst.AgentDefaults.HarnessOverrides {
			if len(o.Env) == 0 {
				continue
			}
			keys := make([]string, 0, len(o.Env))
			for k := range o.Env {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			if in.EnvKeys[inst.Key] == nil {
				in.EnvKeys[inst.Key] = map[string][]string{}
			}
			in.EnvKeys[inst.Key][harness] = keys
		}
	}
	// encoding/json writes map keys sorted and struct fields in order, so
	// the encoding is canonical for these types.
	data, err := json.Marshal(in)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// SettingsInstances returns a copy of the plan's instances in settings
// form, the list the apply writes to server.broker.instances. It carries
// env values and secret targets: never print it, journal it or send it.
func (p *Plan) SettingsInstances() []config.V1RuntimeBrokerInstanceConfig {
	return append([]config.V1RuntimeBrokerInstanceConfig(nil), p.settingsInstances...)
}
