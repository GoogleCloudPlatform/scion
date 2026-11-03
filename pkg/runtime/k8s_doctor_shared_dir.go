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

package runtime

import (
	"fmt"
	"sort"
	"strings"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// sharedDirStorageClassCheck names the doctor check of the configured
	// shared_dir_storage_class.
	sharedDirStorageClassCheck = "shared-dir-storage-class"
	// sharedDirClaimClassesCheck names the doctor check of existing per-dir
	// shared-dir claims whose class differs from the configured one.
	sharedDirClaimClassesCheck = "shared-dir-claim-classes"
	// sharedDirClaimClassesListMax caps the claims the claim-classes check
	// names.
	sharedDirClaimClassesListMax = 10
	// sharedDirClaimDocsURL is the documentation section on starts that need
	// a new shared-dir claim.
	sharedDirClaimDocsURL = "https://googlecloudplatform.github.io/scion/hosted/ha/kubernetes/#starts-that-need-a-new-claim"
)

// checkSharedDirStorageClass checks the settings-level shared_dir_storage_class
// (opts.SharedDirStorageClass). It is skipped when shared dirs are served from
// the project storage share, is information when no class is set (a start that
// needs a new claim is then refused), fails when the StorageClass does not
// exist, and warns when it cannot be read. A StorageClass that exists passes:
// whether it can provision ReadWriteMany volumes is not verified.
func (r *KubernetesRuntime) checkSharedDirStorageClass(opts DiagnosticOpts) CheckResult {
	if opts.SharedDirStorageShared {
		return CheckResult{
			Name:    sharedDirStorageClassCheck,
			Status:  "skip",
			Message: "Shared dirs are served from the project storage share; shared_dir_storage_class is not used",
		}
	}
	class := opts.SharedDirStorageClass
	if class == "" {
		return CheckResult{
			Name:   sharedDirStorageClassCheck,
			Status: "info",
			Message: "No shared_dir_storage_class is set in settings: a start that needs a new shared-dir claim is refused; " +
				"existing claims are reused",
			Remediation: "Configure server.project_storage backend nfs, or set shared_dir_storage_class to a ReadWriteMany class. See " +
				sharedDirClaimDocsURL,
		}
	}

	ctx, cancel := r.diagnosticContext()
	defer cancel()
	sc, err := r.Client.Clientset.StorageV1().StorageClasses().Get(ctx, class, metav1.GetOptions{})
	switch {
	case k8serrors.IsNotFound(err):
		return CheckResult{
			Name:    sharedDirStorageClassCheck,
			Status:  "fail",
			Message: fmt.Sprintf("StorageClass %q (shared_dir_storage_class in settings) does not exist: new shared-dir claims would stay Pending", class),
			Remediation: fmt.Sprintf("Create a StorageClass named %q that can provision ReadWriteMany volumes, "+
				"or set shared_dir_storage_class to an existing ReadWriteMany class", class),
		}
	case k8serrors.IsForbidden(err):
		return CheckResult{
			Name:   sharedDirStorageClassCheck,
			Status: "warn",
			Message: fmt.Sprintf("Could not verify StorageClass %q (shared_dir_storage_class in settings): StorageClasses are cluster-scoped, "+
				"and a namespaced Role cannot read them", class),
			Remediation: "To have doctor verify it, grant get on storageclasses (storage.k8s.io) through a ClusterRole; " +
				"the Runtime Broker itself does not need it",
		}
	case err != nil:
		return CheckResult{
			Name:    sharedDirStorageClassCheck,
			Status:  "warn",
			Message: fmt.Sprintf("Could not verify StorageClass %q (shared_dir_storage_class in settings): %v", class, err),
		}
	}
	return CheckResult{
		Name:   sharedDirStorageClassCheck,
		Status: "pass",
		Message: fmt.Sprintf("StorageClass %q (shared_dir_storage_class in settings) exists, provisioner %s; "+
			"it must support ReadWriteMany (not verified)", class, sc.Provisioner),
	}
}

// checkSharedDirClaimClasses lists the existing per-dir shared-dir claims in
// namespace (label scion.shared-dir) whose storage class differs from the
// settings-level shared_dir_storage_class, using the same comparison as the
// start-time warning (storageClassMismatch). Existing claims are never
// changed, so such a claim keeps its old class. It reports only when a class
// is set and shared dirs use per-dir claims (ok false otherwise), and it never
// fails: differing claims, or claims it cannot list, are a warning.
func (r *KubernetesRuntime) checkSharedDirClaimClasses(namespace string, opts DiagnosticOpts) (CheckResult, bool) {
	class := opts.SharedDirStorageClass
	if opts.SharedDirStorageShared || class == "" {
		return CheckResult{}, false
	}

	ctx, cancel := r.diagnosticContext()
	defer cancel()
	list, err := r.Client.Clientset.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{LabelSelector: sharedDirClaimLabel})
	if k8serrors.IsForbidden(err) {
		return CheckResult{
			Name:    sharedDirClaimClassesCheck,
			Status:  "warn",
			Message: fmt.Sprintf("Could not list shared-dir claims in namespace %q: listing persistentvolumeclaims is not allowed", namespace),
			Remediation: "To have doctor check existing claims, grant list on persistentvolumeclaims in this namespace; " +
				"the Runtime Broker also uses it, with delete, to release a deleted project's shared-dir claims",
		}, true
	}
	if err != nil {
		return CheckResult{
			Name:    sharedDirClaimClassesCheck,
			Status:  "warn",
			Message: fmt.Sprintf("Could not list shared-dir claims in namespace %q: %v", namespace, err),
		}, true
	}

	var differing []string
	for i := range list.Items {
		pvc := &list.Items[i]
		existing, pending, differs := storageClassMismatch(pvc, class)
		if !differs {
			continue
		}
		if existing == "" {
			existing = "none"
		}
		entry := fmt.Sprintf("%s (class %s", pvc.Name, existing)
		if pending {
			entry += ", Pending"
		}
		differing = append(differing, entry+")")
	}
	if len(differing) == 0 {
		var msg string
		switch n := len(list.Items); n {
		case 0:
			msg = fmt.Sprintf("No shared-dir claims in namespace %q", namespace)
		case 1:
			msg = fmt.Sprintf("The 1 shared-dir claim in namespace %q uses StorageClass %q", namespace, class)
		default:
			msg = fmt.Sprintf("All %s in namespace %q use StorageClass %q", sharedDirClaimCount(n), namespace, class)
		}
		return CheckResult{
			Name:    sharedDirClaimClassesCheck,
			Status:  "pass",
			Message: msg,
		}, true
	}

	sort.Strings(differing)
	total := len(differing)
	shown := differing
	if total > sharedDirClaimClassesListMax {
		shown = differing[:sharedDirClaimClassesListMax]
	}
	verb := "use"
	if total == 1 {
		verb = "uses"
	}
	msg := fmt.Sprintf("%s in namespace %q %s a storage class other than %q: %s",
		sharedDirClaimCount(total), namespace, verb, class, strings.Join(shown, ", "))
	if total > len(shown) {
		msg += fmt.Sprintf(", and %d more", total-len(shown))
	}
	return CheckResult{
		Name:    sharedDirClaimClassesCheck,
		Status:  "warn",
		Message: msg,
		Remediation: "Existing claims are never changed; a new shared_dir_storage_class applies only to new claims. " +
			"A different class can be expected: a template's or agent's kubernetes block, or another profile using this namespace, " +
			"may set it. Delete a claim only if its class is really wrong, and only while no agent uses it (its data is lost); " +
			"the next start creates it again",
	}, true
}

// sharedDirClaimCount is "1 shared-dir claim" or "N shared-dir claims".
func sharedDirClaimCount(n int) string {
	if n == 1 {
		return "1 shared-dir claim"
	}
	return fmt.Sprintf("%d shared-dir claims", n)
}
