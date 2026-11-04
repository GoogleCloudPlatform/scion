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
	"encoding/json"
	"sort"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Presentation is the publishable form of p that a legacy Runtime Broker
// reports to the Hub on every heartbeat. Instances are p's instances, which
// BuildLocal builds only through config.PresentInstance; findings carry
// code, severity, key path and profiles, never the converter's message or
// the fixed text; the profile map holds the mapped profiles only.
//
// The result fits the api.ConversionPlan* caps. Parts are dropped whole,
// never cut: an element holding an over-long string, then, while a count
// or the encoded size is over its cap, notice findings, then blocking
// findings, then instances (each with the profile map entries naming it),
// then profile map entries. Any drop adds one blocking plan_truncated
// finding, so a truncated plan never looks convertible.
func Presentation(p *Plan, computedAt time.Time) *api.ConversionPlanPresentation {
	out := &api.ConversionPlanPresentation{
		SchemaVersion: api.ConversionPlanPresentationSchemaVersion,
		Instances:     []api.RuntimeTargetPresentation{},
		ProfileMap:    map[string]string{},
		Findings:      []api.ConversionPlanFinding{},
		ComputedAt:    computedAt.UTC(),
	}
	if p == nil {
		return out
	}
	out.Instances = append(out.Instances, p.Instances...)
	for _, m := range p.ProfileMap {
		if m.Instance != "" {
			out.ProfileMap[m.Profile] = m.Instance
		}
	}
	for _, f := range p.Findings {
		out.Findings = append(out.Findings, api.ConversionPlanFinding{
			Code:     f.Code,
			Severity: f.Severity,
			Path:     f.Path,
			Profiles: append([]string{}, nonNil(f.Profiles)...),
		})
	}
	trimToCaps(out)
	return out
}

// trimToCaps drops whole parts of pres until it fits the caps, and marks a
// trimmed plan with a blocking plan_truncated finding.
func trimToCaps(pres *api.ConversionPlanPresentation) {
	truncated := false

	// Elements holding a string over the per-string cap.
	keptInstances := pres.Instances[:0]
	for _, inst := range pres.Instances {
		if fits(inst) {
			keptInstances = append(keptInstances, inst)
			continue
		}
		truncated = true
		dropMappingsTo(pres, inst.Key)
	}
	pres.Instances = keptInstances
	keptFindings := pres.Findings[:0]
	for _, f := range pres.Findings {
		if fits(f) {
			keptFindings = append(keptFindings, f)
		} else {
			truncated = true
		}
	}
	pres.Findings = keptFindings
	for profile, key := range pres.ProfileMap {
		if len(profile) > api.ConversionPlanMaxStringBytes || len(key) > api.ConversionPlanMaxStringBytes {
			delete(pres.ProfileMap, profile)
			truncated = true
		}
	}

	// Counts. Findings are trimmed only when they exceed the cap; then one
	// slot is left for plan_truncated.
	if len(pres.Findings) > api.ConversionPlanMaxFindings {
		truncated = true
		for len(pres.Findings) > api.ConversionPlanMaxFindings-1 {
			dropOneFinding(pres)
		}
	}
	for len(pres.Instances) > api.ConversionPlanMaxInstances {
		dropLastInstance(pres)
		truncated = true
	}
	for len(pres.ProfileMap) > api.ConversionPlanMaxProfileMap {
		dropLastMapping(pres)
		truncated = true
	}

	// Encoded size, measured with the plan_truncated finding in place
	// when one is (or will be) added.
	for encodedSize(pres, truncated) > api.ConversionPlanMaxBytes {
		switch {
		case len(pres.Findings) > 0:
			dropOneFinding(pres)
		case len(pres.Instances) > 0:
			dropLastInstance(pres)
		case len(pres.ProfileMap) > 0:
			dropLastMapping(pres)
		default:
			// Nothing left to drop; an empty plan always fits.
			return
		}
		truncated = true
	}
	if truncated {
		// A plan trimmed for another reason may still hold exactly the cap
		// of findings; make room for the marker.
		for len(pres.Findings) > api.ConversionPlanMaxFindings-1 {
			dropOneFinding(pres)
		}
		pres.Findings = append(pres.Findings, truncatedFinding())
	}
}

func truncatedFinding() api.ConversionPlanFinding {
	return api.ConversionPlanFinding{
		Code:     api.ConversionPlanTruncatedCode,
		Severity: config.ConversionSeverityBlocking,
		Path:     "",
		Profiles: []string{},
	}
}

// fits reports whether every string in v is within the per-string cap.
func fits(v any) bool {
	b, err := json.Marshal(v)
	return err == nil && api.ConversionPlanStringsFit(b)
}

// encodedSize is the encoded size of pres, plus the plan_truncated
// finding when withTruncated is set.
func encodedSize(pres *api.ConversionPlanPresentation, withTruncated bool) int {
	c := *pres
	if withTruncated {
		c.Findings = append(append([]api.ConversionPlanFinding{}, pres.Findings...), truncatedFinding())
	}
	b, err := json.Marshal(&c)
	if err != nil {
		return api.ConversionPlanMaxBytes + 1
	}
	return len(b)
}

// dropOneFinding drops the last notice finding, or, when none is left, the
// last blocking finding.
func dropOneFinding(pres *api.ConversionPlanPresentation) {
	for i := len(pres.Findings) - 1; i >= 0; i-- {
		if pres.Findings[i].Severity != config.ConversionSeverityBlocking {
			pres.Findings = append(pres.Findings[:i], pres.Findings[i+1:]...)
			return
		}
	}
	if n := len(pres.Findings); n > 0 {
		pres.Findings = pres.Findings[:n-1]
	}
}

// dropLastInstance drops the last instance and the profile map entries
// that name it.
func dropLastInstance(pres *api.ConversionPlanPresentation) {
	n := len(pres.Instances)
	if n == 0 {
		return
	}
	key := pres.Instances[n-1].Key
	pres.Instances = pres.Instances[:n-1]
	dropMappingsTo(pres, key)
}

func dropMappingsTo(pres *api.ConversionPlanPresentation, key string) {
	for profile, k := range pres.ProfileMap {
		if k == key {
			delete(pres.ProfileMap, profile)
		}
	}
}

// dropLastMapping drops the profile map entry with the greatest profile
// name.
func dropLastMapping(pres *api.ConversionPlanPresentation) {
	names := make([]string, 0, len(pres.ProfileMap))
	for name := range pres.ProfileMap {
		names = append(names, name)
	}
	if len(names) == 0 {
		return
	}
	sort.Strings(names)
	delete(pres.ProfileMap, names[len(names)-1])
}
