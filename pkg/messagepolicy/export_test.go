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

package messagepolicy

import "github.com/GoogleCloudPlatform/scion/pkg/ruleengine"

// NewDefault builds a standalone *Evaluator backed by a fresh *ruleengine.Engine
// with the message_policy v1 CEL environment registered and ConsumerName granted
// DialectCEL (no I/O). Compiled only for package messagepolicy tests.
func NewDefault() (*Evaluator, error) {
	celEng, err := NewCELEngine()
	if err != nil {
		return nil, err
	}
	celDialect, err := ruleengine.NewCELDialect(celEng)
	if err != nil {
		return nil, err
	}
	re := ruleengine.NewEngine()
	if err := re.Register(celDialect); err != nil {
		return nil, err
	}
	if err := re.AllowConsumerDialects(ConsumerName, ruleengine.Grant(ruleengine.DialectCEL)); err != nil {
		return nil, err
	}
	return New(re)
}
