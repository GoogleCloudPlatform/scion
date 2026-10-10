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
 * Test helpers for the Create Agent page, whose Additional Options are
 * rendered by the embedded <scion-agent-config-form> and whose GCP identity
 * state lives in the page's GcpIdentityState (ptone/scion#3974).
 */

import type { GcpIdentityState } from '../../../shared/gcp-identity-state.js';

/** The GcpIdentityState fields a test may read or set through createInternals. */
const GCP_FIELDS = new Set<string>([
  'gcpMetadataMode',
  'gcpServiceAccountId',
  'gcpIdentityUserSet',
  'gcpUserBlockSuspended',
  'defaultGcpMetadataMode',
  'defaultGcpServiceAccountId',
  'gcpServiceAccounts',
  'projectGCPIdentityDefaultMode',
  'projectGCPIdentityDefaultApplied',
  'projectGCPIdentityProfileDefaults',
]);

/**
 * A view of the page in which the GCP identity fields read and write the
 * page's GcpIdentityState, and everything else reads and writes the page
 * (TS privacy is compile-time only). Setting a GCP field re-applies the
 * Kubernetes rule, as a change of the page's own state used to.
 */
export function createInternals<T>(el: HTMLElement): T {
  const page = el as unknown as Record<string, unknown> & { gcp: GcpIdentityState };
  return new Proxy(page, {
    get(target, key) {
      if (typeof key === 'string' && GCP_FIELDS.has(key)) {
        return (target.gcp as unknown as Record<string, unknown>)[key];
      }
      const v = Reflect.get(target, key, target) as unknown;
      return typeof v === 'function' ? (v as (...a: unknown[]) => unknown).bind(target) : v;
    },
    set(target, key, value) {
      if (typeof key === 'string' && GCP_FIELDS.has(key)) {
        (target.gcp as unknown as Record<string, unknown>)[key] = value;
        target.gcp.normalize();
        target.gcp.notify();
        return true;
      }
      return Reflect.set(target, key, value, target);
    },
  }) as unknown as T;
}

/** The embedded form's shadow root, where the Additional Options render. */
export function formRoot(el: Element): ShadowRoot {
  const form = el.shadowRoot?.querySelector('scion-agent-config-form');
  if (!form?.shadowRoot) throw new Error('scion-agent-config-form not rendered');
  return form.shadowRoot;
}

/** The form-field wrapper whose label text is label, in the form. */
export function formField(el: Element, label: string): Element | null {
  const fields = Array.from(formRoot(el).querySelectorAll('.field'));
  return (
    fields.find((f) => {
      const text = f.querySelector('label')?.textContent?.replace(/\s+/g, ' ').trim() ?? '';
      return text === label || text.startsWith(`${label} `);
    }) ?? null
  );
}
