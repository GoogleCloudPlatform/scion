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

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
)

type fakeIODialectForP1Test struct {
	evalCalls atomic.Int32
}

func (d *fakeIODialectForP1Test) Name() string { return "fake_io_p1" }

func (d *fakeIODialectForP1Test) Declares() ruleengine.Capabilities {
	return ruleengine.Capabilities{
		PerformsIO:  true,
		MaxEvalTime: time.Second,
		ResultKinds: []ruleengine.Kind{ruleengine.KindFilter},
	}
}

func (d *fakeIODialectForP1Test) Compile(_ context.Context, rule string, kind ruleengine.Kind, env ruleengine.EnvironmentRef) (ruleengine.Compiled, error) {
	return ruleengine.NewCompiledWithIdentity(env.Consumer, d.Name(), kind, "fake-io-"+rule, rule), nil
}

func (d *fakeIODialectForP1Test) Evaluate(_ context.Context, c ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
	d.evalCalls.Add(1)
	return ruleengine.Decision{Kind: c.Kind(), Bool: true}, nil
}

// TestDriveChainRun_FailsClosedOnRealIOPendingStep verifies Review O5: when a
// real ruleengine.ChainRun contains an IO-dialect element and Next() returns a
// PendingStep, driveChainRun fails closed without invoking EvaluatePending or
// executing the IO dialect, and attaches content-free ChainProvenance.
func TestDriveChainRun_FailsClosedOnRealIOPendingStep(t *testing.T) {
	dial := &fakeIODialectForP1Test{}
	eng := ruleengine.NewEngine()
	require.NoError(t, eng.Register(dial))
	require.NoError(t, eng.AllowConsumerDialects(ConsumerName, ruleengine.GrantIO(dial.Name())))

	compiled, err := eng.Compile(context.Background(), dial.Name(), "step0", ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: ConsumerName,
		Name:     ChainEnvironmentName,
	})
	require.NoError(t, err)

	dom := ruleengine.Domain{Ctx: context.Background(), Budget: ruleengine.Budget{MaxEvalTime: time.Second}}
	run, err := eng.NewChainRun(
		ruleengine.ChainDomains{Inline: dom, IO: dom},
		ruleengine.Chain{Elements: []ruleengine.ChainElement{
			{Index: 0, Dialect: dial.Name(), Kind: ruleengine.KindFilter, Compiled: compiled, Role: StepRoleViolation},
		}},
		ruleengine.Input{Activation: map[string]any{}},
		ruleengine.ChainLimits{MaxElements: MaxChainSteps, MaxTraceBytes: 4096},
		nil,
		ruleengine.NoopEnricher{},
		nil,
	)
	require.NoError(t, err)

	cls := driveChainRun(run, []ChainStepInput{{ID: "io_step", Role: StepRoleViolation, Dialect: dial.Name()}}, "", "")
	assert.Equal(t, ClassFailed, cls.Class)
	assert.Equal(t, int32(0), dial.evalCalls.Load(), "IO dialect must never be evaluated on P1 path")
	require.NotNil(t, cls.ChainProvenance)
	assert.Equal(t, 0, cls.ChainProvenance.Through)
	assert.Equal(t, ChainVerdictError, cls.ChainProvenance.Verdict)
	assert.Equal(t, "failed", cls.ChainProvenance.EngineClass)
}
