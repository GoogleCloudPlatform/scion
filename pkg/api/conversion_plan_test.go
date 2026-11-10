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

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func encodePlan(t *testing.T, p ConversionPlanPresentation) []byte {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func basePlan() ConversionPlanPresentation {
	return ConversionPlanPresentation{
		SchemaVersion: ConversionPlanPresentationSchemaVersion,
		Instances:     []RuntimeTargetPresentation{{SchemaVersion: 1, Key: "k8s-agents", Name: "k8s-agents", Target: &PresentedTarget{Type: "kubernetes"}}},
		ProfileMap:    map[string]string{"a": "k8s-agents"},
		Findings:      []ConversionPlanFinding{{Code: "profile_conflict", Severity: "notice", Path: "profiles.*", Profiles: []string{"a", "b"}}},
		ComputedAt:    time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC),
	}
}

func TestCheckConversionPlanCaps_InCap(t *testing.T) {
	if err := CheckConversionPlanCaps(encodePlan(t, basePlan())); err != nil {
		t.Fatalf("in-cap plan refused: %v", err)
	}
}

func TestCheckConversionPlanCaps_EachCap(t *testing.T) {
	long := strings.Repeat("z", ConversionPlanMaxStringBytes+1)
	cases := map[string]func(p *ConversionPlanPresentation){
		"instances": func(p *ConversionPlanPresentation) {
			for i := 0; i < ConversionPlanMaxInstances; i++ {
				p.Instances = append(p.Instances, RuntimeTargetPresentation{Key: fmt.Sprint(i)})
			}
		},
		"findings": func(p *ConversionPlanPresentation) {
			for i := 0; i < ConversionPlanMaxFindings; i++ {
				p.Findings = append(p.Findings, ConversionPlanFinding{Code: "c"})
			}
		},
		"profileMap": func(p *ConversionPlanPresentation) {
			for i := 0; i < ConversionPlanMaxProfileMap; i++ {
				p.ProfileMap[fmt.Sprint("p", i)] = "k"
			}
		},
		"string value": func(p *ConversionPlanPresentation) { p.Findings[0].Path = long },
		"profile map key": func(p *ConversionPlanPresentation) {
			p.ProfileMap[long] = "k"
		},
		"nested map key": func(p *ConversionPlanPresentation) {
			p.Instances[0].Target.NodeSelector = map[string]string{long: "v"}
		},
		"nested list string": func(p *ConversionPlanPresentation) {
			p.Instances[0].Target.Tolerations = []K8sToleration{{Key: long}}
		},
		"encoded size": func(p *ConversionPlanPresentation) {
			pad := strings.Repeat("s", ConversionPlanMaxStringBytes)
			for i := 0; len(encodePlan(t, *p)) <= ConversionPlanMaxBytes; i++ {
				p.Findings = append(p.Findings, ConversionPlanFinding{Code: "c", Path: pad})
				if i > ConversionPlanMaxFindings {
					t.Fatal("could not exceed the byte cap within the findings cap")
				}
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := basePlan()
			mutate(&p)
			err := CheckConversionPlanCaps(encodePlan(t, p))
			if !errors.Is(err, ErrConversionPlanOverCap) {
				t.Fatalf("got %v, want ErrConversionPlanOverCap", err)
			}
			if strings.Contains(err.Error(), "zzzz") || strings.Contains(err.Error(), "ssss") {
				t.Fatalf("the error quotes content: %v", err)
			}
		})
	}
}

// TestCheckConversionPlanCaps_ByteCapBeforeDecode: an oversized body is
// refused on its length, even when it would not decode.
func TestCheckConversionPlanCaps_ByteCapBeforeDecode(t *testing.T) {
	body := []byte("{" + strings.Repeat(" ", ConversionPlanMaxBytes))
	if err := CheckConversionPlanCaps(body); !errors.Is(err, ErrConversionPlanOverCap) {
		t.Fatalf("got %v, want the byte cap", err)
	}
}

func TestCheckConversionPlanCaps_DecodeError(t *testing.T) {
	err := CheckConversionPlanCaps([]byte(`{"instances": "PLANTED-x"`))
	if err == nil || errors.Is(err, ErrConversionPlanOverCap) || strings.Contains(err.Error(), "PLANTED") {
		t.Fatalf("got %v, want a content-free decode error", err)
	}
}
