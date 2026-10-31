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
	"reflect"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"k8s.io/apimachinery/pkg/util/validation"
)

// RuntimeSaveChecks checks the Kubernetes placement and identity fields of
// a settings save (ptone/scion#6580): node_selector, tolerations,
// runtime_class_name, kubernetes_service_account_mappings and
// kubernetes_block_service_account on runtime entries and profiles,
// priority_class_name on runtime entries, and the
// agreement of sibling runtime entries (entries targeting the same cluster
// and namespace, LiteralSiblingGroups). before holds the stored values and
// after the values the save would store.
//
// A problem the save causes is an error (the save is refused): an invalid
// value in a field the save sets or changes, on a new entry, or a sibling
// disagreement in a group where the save adds an entry or changes a member's
// placement, context or namespace. A problem that was already stored and
// that the save does not touch is a warning (the save is accepted), so a
// pre-existing problem never locks an operator out of unrelated edits.
// Both lists are sorted by path.
func RuntimeSaveChecks(before, after map[string]V1RuntimeConfig, beforeProfiles, afterProfiles map[string]V1ProfileConfig) (errs, warnings []ValidationError) {
	add := func(touched bool, e ValidationError) {
		if touched {
			errs = append(errs, e)
		} else {
			warnings = append(warnings, e)
		}
	}
	for name, rt := range after {
		old, existed := before[name]
		path := "runtimes." + name
		if rt.PriorityClassName != "" {
			if msgs := validation.IsDNS1123Subdomain(rt.PriorityClassName); len(msgs) > 0 {
				add(!existed || old.PriorityClassName != rt.PriorityClassName, ValidationError{Path: path + ".priority_class_name",
					Message: fmt.Sprintf("%q is not a valid PriorityClass name: %s", rt.PriorityClassName, strings.Join(msgs, "; "))})
			}
		}
		checkIdentityFields(path, rt.NodeSelector, rt.Tolerations, rt.RuntimeClassName, rt.KubernetesServiceAccountMappings, rt.KubernetesBlockServiceAccount,
			existed, old.NodeSelector, old.Tolerations, old.RuntimeClassName, old.KubernetesServiceAccountMappings, old.KubernetesBlockServiceAccount, add)
	}
	for name, p := range afterProfiles {
		old, existed := beforeProfiles[name]
		path := "profiles." + name
		checkIdentityFields(path, p.NodeSelector, p.Tolerations, p.RuntimeClassName, p.KubernetesServiceAccountMappings, p.KubernetesBlockServiceAccount,
			existed, old.NodeSelector, old.Tolerations, old.RuntimeClassName, old.KubernetesServiceAccountMappings, old.KubernetesBlockServiceAccount, add)
	}
	for _, group := range LiteralSiblingGroups(after) {
		if len(group) < 2 {
			continue
		}
		// SiblingPlacementAgrees is an equivalence (equal selectors, equal
		// toleration sets, equal runtime class), so comparing the first
		// member with each other member finds any disagreement in the
		// group; all pairs need not be compared.
		first := group[0]
		var other string
		for _, o := range group[1:] {
			if !SiblingPlacementAgrees(after[first], after[o]) {
				other = o
				break
			}
		}
		if other == "" {
			continue
		}
		touched := false
		for _, n := range group {
			old, existed := before[n]
			cur := after[n]
			if !existed || !siblingPlacementEqual(old, cur) || old.Context != cur.Context || old.Namespace != cur.Namespace || old.Type != cur.Type || old.Host != cur.Host {
				touched = true
				break
			}
		}
		add(touched, ValidationError{Path: "runtimes." + first, Message: SiblingDisagreementCause(first, other)})
	}
	sortValidationErrors(errs)
	sortValidationErrors(warnings)
	return errs, warnings
}

// siblingPlacementEqual reports whether a save left an entry's placement
// unchanged. It uses the same equality as SiblingPlacementAgrees: a nil and
// an empty node selector are equal, and tolerations compare as sets. Stored
// settings decode an absent field as nil and an empty one as empty, so a
// stricter comparison would mark an untouched entry as changed and turn a
// stored sibling disagreement into a refusal.
func siblingPlacementEqual(a, b V1RuntimeConfig) bool {
	return SiblingPlacementAgrees(a, b)
}

// checkIdentityFields checks one entry's placement and identity fields and
// reports each problem through add, as touched when the save sets or changes
// that field (or the entry is new).
func checkIdentityFields(path string, sel map[string]string, tols []api.K8sToleration, class string, mappings map[string]string, blockSA string,
	existed bool, oldSel map[string]string, oldTols []api.K8sToleration, oldClass string, oldMappings map[string]string, oldBlockSA string,
	add func(bool, ValidationError)) {
	changed := func(a, b any) bool { return !existed || !reflect.DeepEqual(a, b) }
	if err := ValidateNodeSelector(sel); err != nil {
		add(changed(sel, oldSel), ValidationError{Path: path + ".node_selector", Message: err.Error()})
	}
	if err := ValidateTolerations(tols); err != nil {
		add(changed(tols, oldTols), ValidationError{Path: path + ".tolerations", Message: err.Error()})
	}
	if err := ValidateRuntimeClassName(class); err != nil {
		add(changed(class, oldClass), ValidationError{Path: path + ".runtime_class_name", Message: err.Error()})
	}
	if err := ValidateKubernetesServiceAccountMappings(mappings); err != nil {
		add(changed(mappings, oldMappings), ValidationError{Path: path + ".kubernetes_service_account_mappings", Message: err.Error()})
	}
	if blockSA != "" {
		if err := ValidateKubernetesBlockServiceAccount(blockSA); err != nil {
			add(changed(blockSA, oldBlockSA), ValidationError{Path: path + ".kubernetes_block_service_account", Message: err.Error()})
		}
	}
}
