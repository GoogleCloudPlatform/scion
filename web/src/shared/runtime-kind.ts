/**
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

/**
 * Shared classification for "is this runtime profile/broker Kubernetes",
 * used by every surface that hides or disables the "block" GCP identity mode
 * for a Kubernetes target (ptone/scion#2328 Phase 2).
 *
 * A `BrokerProfile.type` is the settings *runtime key* the profile's
 * `runtime:` field references (pkg/runtimebroker/handlers.go,
 * cmd/server_broker.go build it directly from that map), not a resolved
 * type. The runtime factory (pkg/runtime/factory.go) accepts "k8s" as an
 * alias for "kubernetes" and normalizes "remote" to "kubernetes" before
 * dispatch, so all three spellings must be treated as the same runtime here
 * — matching literally on "kubernetes" alone misses profiles registered
 * under either alias. Phase 1 (pkg/runtimebroker/start_context.go,
 * ptone/scion#2338) classifies by this same string set against the resolved
 * runtime's Name(), so the two cannot drift on which spellings count.
 *
 * This matches the profile's runtime type exactly — profile names (e.g.
 * "my-cluster") are never inspected, only the runtime key the profile's
 * `runtime:` field references: a profile named "my-cluster" with
 * `runtime: kubernetes` correctly reports type "kubernetes", because
 * "kubernetes" is the runtime key, not the profile's own name. The gap is a
 * custom-named *runtime entry* —
 * `runtimes.gke-prod: {type: kubernetes}` referenced by `runtime: gke-prod`
 * reports type "gke-prod" and is missed here. The same gap runs in reverse: a
 * runtime key spelled "kubernetes"/"k8s"/"remote" with an explicit
 * `type: docker` (or any other non-Kubernetes type) is misclassified as
 * Kubernetes here, though it dispatches as that other type. Both need the
 * broker to report its resolved type instead of the profile's runtime key;
 * out of scope here.
 */
const KUBERNETES_RUNTIME_TYPES = new Set(['kubernetes', 'k8s', 'remote']);

/** A runtime profile, or just enough of one to classify its runtime kind. */
export interface RuntimeKindProfile {
  name: string;
  type: string;
  available: boolean;
}

/** A runtime broker, or just enough of one to classify its runtime kind. */
export interface RuntimeKindBroker {
  profiles?: RuntimeKindProfile[];
}

/** Whether a profile's `type` names the Kubernetes runtime, under any accepted spelling. */
export function isKubernetesRuntimeType(type: string | undefined): boolean {
  return !!type && KUBERNETES_RUNTIME_TYPES.has(type);
}

/**
 * Whether every profile registered on broker is Kubernetes. A broker with no
 * registered profiles is not considered Kubernetes-only — there is nothing to
 * confirm the type from, and this must not guess.
 */
export function isBrokerKubernetesOnly(broker: RuntimeKindBroker | undefined): boolean {
  const profiles = broker?.profiles ?? [];
  if (profiles.length === 0) return false;
  return profiles.every((p) => isKubernetesRuntimeType(p.type));
}

/**
 * Whether a specific broker/profile selection (as a create or configure
 * request would carry it) is reliably known to resolve to Kubernetes.
 *
 * With a profile name chosen, only that profile's type decides the answer.
 * With no profile chosen, this is true only when every *available* profile
 * on the broker is Kubernetes — a broker with no available profiles, or a
 * mix of runtime types and no profile picked yet, is not reliably known and
 * reads as false (not a guess).
 */
export function isTargetKubernetesOnly(
  broker: RuntimeKindBroker | undefined,
  profileName: string
): boolean {
  if (!broker) return false;
  if (profileName) {
    const selected = broker.profiles?.find((p) => p.name === profileName);
    return isKubernetesRuntimeType(selected?.type);
  }
  const available = broker.profiles?.filter((p) => p.available) ?? [];
  if (available.length === 0) return false;
  return available.every((p) => isKubernetesRuntimeType(p.type));
}
