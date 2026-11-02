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

package config

import (
	"fmt"
	"regexp"
	"sort"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Resource quantity floors for memory and disk (ptone/scion#6581). A
// Kubernetes quantity without a unit is a byte count, so "16" means 16
// bytes: such a limit starts the pod but breaks it (the in-memory volumes
// are sized from the memory limit, so writes fail with ENOSPC).
var (
	// minResourceBytes is the smallest memory or disk value accepted.
	minResourceBytes = resource.MustParse("1Mi")
	// lowMemoryBytes is the memory value below which a save warns.
	lowMemoryBytes = resource.MustParse("64Mi")
)

// unitlessQuantity matches a quantity written as a bare number, with no
// suffix or exponent.
var unitlessQuantity = regexp.MustCompile(`^[+-]?([0-9]+\.?[0-9]*|\.[0-9]+)$`)

// ValidateResourceQuantities checks the cpu, memory and disk quantities of
// default_resources, every profiles.NAME.resources and every
// profiles.NAME.harness_overrides.HARNESS.resources. It returns the errors
// a save must reject and the warnings it reports but accepts, each sorted
// by path. See ValidateResourceSpec for the rules.
func ValidateResourceQuantities(defaultResources *api.ResourceSpec, profiles map[string]V1ProfileConfig) (errs, warnings []ValidationError) {
	add := func(path string, spec *api.ResourceSpec) {
		e, w := ValidateResourceSpec(path, spec)
		errs = append(errs, e...)
		warnings = append(warnings, w...)
	}
	add("default_resources", defaultResources)
	for name, p := range profiles {
		add("profiles."+name+".resources", p.Resources)
		for harness, ho := range p.HarnessOverrides {
			add("profiles."+name+".harness_overrides."+harness+".resources", ho.Resources)
		}
	}
	sortValidationErrors(errs)
	sortValidationErrors(warnings)
	return errs, warnings
}

// ValidateResourceSpec checks one resource spec, whose settings path is
// path (for example "profiles.gke.resources"). Empty values are unset and
// pass. Errors:
//   - a value that is not a Kubernetes quantity, or is zero or negative;
//   - memory or disk below 1Mi, whatever the unit (this catches "16",
//     which is 16 bytes);
//   - a cpu or memory request above its limit (Kubernetes rejects the pod).
//
// Warnings (accepted):
//   - memory or disk of 1Mi or more written without a unit (a byte count
//     such as "2147483648");
//   - memory below 64Mi.
//
// CPU without a unit is a number of cores and is fine.
func ValidateResourceSpec(path string, spec *api.ResourceSpec) (errs, warnings []ValidationError) {
	if spec == nil {
		return nil, nil
	}
	type field struct {
		key, value, kind string
	}
	fields := []field{
		{"requests.cpu", spec.Requests.CPU, "cpu"},
		{"requests.memory", spec.Requests.Memory, "memory"},
		{"limits.cpu", spec.Limits.CPU, "cpu"},
		{"limits.memory", spec.Limits.Memory, "memory"},
		{"disk", spec.Disk, "disk"},
	}
	parsed := map[string]resource.Quantity{}
	for _, f := range fields {
		if f.value == "" {
			continue
		}
		p := path + "." + f.key
		q, err := resource.ParseQuantity(f.value)
		if err != nil {
			errs = append(errs, ValidationError{Path: p, Message: fmt.Sprintf("%q is not a Kubernetes quantity; %s", f.value, quantityExample(f.kind))})
			continue
		}
		if q.Sign() <= 0 {
			errs = append(errs, ValidationError{Path: p, Message: fmt.Sprintf("%q must be greater than zero; %s", f.value, quantityExample(f.kind))})
			continue
		}
		if f.kind == "cpu" {
			parsed[f.key] = q
			continue
		}
		unitless := unitlessQuantity.MatchString(f.value)
		if q.Cmp(minResourceBytes) < 0 {
			msg := fmt.Sprintf("%q is below the minimum of 1Mi", f.value)
			if unitless {
				msg = fmt.Sprintf("%q has no unit, so it means %s bytes, below the minimum of 1Mi", f.value, f.value)
			}
			errs = append(errs, ValidationError{Path: p, Message: msg + "; " + quantityExample(f.kind)})
			continue
		}
		parsed[f.key] = q
		if unitless {
			warnings = append(warnings, ValidationError{Path: p, Message: fmt.Sprintf("%q has no unit, so it is a number of bytes (%s); write it with a unit, for example %s", f.value, q.String(), suggestBinaryUnit(q))})
		}
		if f.kind == "memory" && q.Cmp(lowMemoryBytes) < 0 {
			warnings = append(warnings, ValidationError{Path: p, Message: fmt.Sprintf("%q is below 64Mi, too little memory for an agent; did you mean a larger unit, such as 16Gi?", f.value)})
		}
	}
	for _, r := range []string{"cpu", "memory"} {
		req, okReq := parsed["requests."+r]
		lim, okLim := parsed["limits."+r]
		if okReq && okLim && req.Cmp(lim) > 0 {
			errs = append(errs, ValidationError{Path: path + ".requests." + r, Message: fmt.Sprintf("request %q is above the limit %q; a request must not exceed its limit", req.String(), lim.String())})
		}
	}
	return errs, warnings
}

// quantityExample returns the hint for a quantity of the given kind.
func quantityExample(kind string) string {
	switch kind {
	case "cpu":
		return "use a number of cores or millicores, such as 2 or 500m"
	case "disk":
		return "use a quantity with a unit, such as 20Gi"
	default:
		return "use a quantity with a unit, such as 16Gi or 512Mi"
	}
}

// suggestBinaryUnit writes a byte quantity with the largest binary unit
// (Ki, Mi, Gi, Ti) that divides it exactly, or rounded to Mi.
func suggestBinaryUnit(q resource.Quantity) string {
	b := q.Value()
	for _, u := range []struct {
		suffix string
		size   int64
	}{{"Ti", 1 << 40}, {"Gi", 1 << 30}, {"Mi", 1 << 20}, {"Ki", 1 << 10}} {
		if b >= u.size && b%u.size == 0 {
			return fmt.Sprintf("%d%s", b/u.size, u.suffix)
		}
	}
	return fmt.Sprintf("%dMi", (b+(1<<20)/2)>>20)
}

// sortValidationErrors sorts errs by path, then message.
func sortValidationErrors(errs []ValidationError) {
	sort.Slice(errs, func(i, j int) bool {
		if errs[i].Path != errs[j].Path {
			return errs[i].Path < errs[j].Path
		}
		return errs[i].Message < errs[j].Message
	})
}
