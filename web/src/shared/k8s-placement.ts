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
 * Kubernetes placement and identity checks for the Server Config page's
 * runtime editor (ptone/scion#6580). Mirrors the hub's save checks
 * (config.RuntimeSaveChecks: ValidateNodeSelector, ValidateTolerations,
 * ValidateRuntimeClassName, ValidateKubernetesServiceAccountMappings,
 * ValidateKubernetesBlockServiceAccount), which follow the Kubernetes API
 * server's rules. An error blocks Save.
 */

export interface Toleration {
  key?: string;
  operator?: string;
  value?: string;
  effect?: string;
}

export interface K8sRuntimeFields {
  node_selector?: Record<string, string>;
  tolerations?: Toleration[];
  runtime_class_name?: string;
  kubernetes_service_account_mappings?: Record<string, string>;
  kubernetes_block_service_account?: string;
  priority_class_name?: string;
}

export interface PlacementIssue {
  path: string;
  message: string;
}

const NAME_PART = /^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$/;
const DNS1123_SUBDOMAIN = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$/;
const GSA_EMAIL =
  /^[a-z0-9]([a-z0-9-]*[a-z0-9])?@([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+gserviceaccount\.com$/;

/** A Kubernetes qualified name: an optional DNS-1123 subdomain prefix and "/", then a name of at most 63 characters. */
export function isQualifiedName(s: string): boolean {
  const parts = s.split('/');
  if (parts.length > 2) return false;
  const name = parts[parts.length - 1];
  if (!name || name.length > 63 || !NAME_PART.test(name)) return false;
  if (parts.length === 2) {
    const prefix = parts[0];
    if (!prefix || prefix.length > 253 || !DNS1123_SUBDOMAIN.test(prefix)) return false;
  }
  return true;
}

/** A Kubernetes label value: empty, or at most 63 characters of the name form. */
export function isLabelValue(s: string): boolean {
  return s === '' || (s.length <= 63 && NAME_PART.test(s));
}

/** A DNS-1123 subdomain (Kubernetes object names such as RuntimeClass and ServiceAccount). */
export function isDNS1123Subdomain(s: string): boolean {
  return s.length > 0 && s.length <= 253 && DNS1123_SUBDOMAIN.test(s);
}

/** The placement and identity problems of one runtime entry, at runtimes.<name>.<field>. */
export function validateK8sRuntimeFields(name: string, rt: K8sRuntimeFields): PlacementIssue[] {
  const p = `runtimes.${name}`;
  const out: PlacementIssue[] = [];
  for (const [k, v] of Object.entries(rt.node_selector ?? {})) {
    if (!isQualifiedName(k)) {
      out.push({
        path: `${p}.node_selector`,
        message: `key "${k}" is not a valid label key (for example scion.dev/pool)`,
      });
    }
    if (!isLabelValue(v)) {
      out.push({
        path: `${p}.node_selector`,
        message: `value "${v}" for key "${k}" is not a valid label value (letters, digits, - _ ., at most 63)`,
      });
    }
  }
  (rt.tolerations ?? []).forEach((t, i) => {
    const key = t.key ?? '';
    const op = t.operator ?? '';
    const value = t.value ?? '';
    const effect = t.effect ?? '';
    const tp = `${p}.tolerations`;
    if (key !== '') {
      if (!isQualifiedName(key))
        out.push({ path: tp, message: `toleration ${i + 1}: key "${key}" is not a valid key` });
    } else if (op !== 'Exists') {
      out.push({
        path: tp,
        message: `toleration ${i + 1}: the operator must be Exists when the key is empty`,
      });
    }
    if (op === '' || op === 'Equal') {
      if (!isLabelValue(value))
        out.push({
          path: tp,
          message: `toleration ${i + 1}: value "${value}" is not a valid label value`,
        });
    } else if (op === 'Exists') {
      if (value !== '')
        out.push({
          path: tp,
          message: `toleration ${i + 1}: the value must be empty when the operator is Exists`,
        });
    } else {
      out.push({
        path: tp,
        message: `toleration ${i + 1}: operator "${op}" is not supported (use Equal or Exists)`,
      });
    }
    if (!['', 'NoSchedule', 'PreferNoSchedule', 'NoExecute'].includes(effect)) {
      out.push({ path: tp, message: `toleration ${i + 1}: effect "${effect}" is not supported` });
    }
  });
  const named: Array<[keyof K8sRuntimeFields, string]> = [
    ['runtime_class_name', 'RuntimeClass'],
    ['kubernetes_block_service_account', 'Kubernetes ServiceAccount'],
    ['priority_class_name', 'PriorityClass'],
  ];
  for (const [field, kind] of named) {
    const v = rt[field] as string | undefined;
    if (v && !isDNS1123Subdomain(v)) {
      out.push({
        path: `${p}.${field}`,
        message: `"${v}" is not a valid ${kind} name (lowercase letters, digits, - and ., at most 253)`,
      });
    }
  }
  for (const [gsa, ksa] of Object.entries(rt.kubernetes_service_account_mappings ?? {})) {
    if (!GSA_EMAIL.test(gsa)) {
      out.push({
        path: `${p}.kubernetes_service_account_mappings`,
        message: `"${gsa}" is not a lowercase GCP service account email (name@project.iam.gserviceaccount.com)`,
      });
    }
    if (!isDNS1123Subdomain(ksa)) {
      out.push({
        path: `${p}.kubernetes_service_account_mappings`,
        message: `"${ksa}" (for ${gsa}) is not a valid Kubernetes ServiceAccount name`,
      });
    }
  }
  return out;
}
