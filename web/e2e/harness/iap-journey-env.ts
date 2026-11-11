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
 * The configuration of the IAP browser journey (web/e2e/iap-journey,
 * ptone/scion#4240 X5). The journey runs only when an operator opts in and
 * sets its environment; otherwise it skips with the reason
 * iapJourneyConfig() returns. The target hub, the driver principal and the
 * IAP audience come from the scion target-hub profile (`scion hub profile
 * show <name> --json`), read by profileTarget(). None of these values is a
 * credential.
 */

/** What the journey needs from the environment. */
export interface IapJourneyConfig {
  /** The scion target-hub profile of the IAP-fronted test hub. */
  profile: string;
  /** The run label for the journey's test identity. */
  run: string;
  /** The scion binary (default "scion" on PATH). */
  scionBin: string;
}

/** What the journey reads from the profile. */
export interface IapJourneyTarget {
  /** The target hub's origin, e.g. https://test-hub.example. */
  hubURL: string;
  /** The driver principal: the profile's IAP service account (auth.proxy.driverPrincipals). */
  driverServiceAccount: string;
  /** The IAP OAuth client ID: the profile's IAP audience (auth.proxy.driverTokenAudience). */
  iapAudience: string;
}

/** The variables the journey requires besides the opt-in IAP_JOURNEY=1. */
export const IAP_JOURNEY_REQUIRED = [
  'IAP_JOURNEY_PROFILE',
  'SCION_METADATA_MODE',
  'GCE_METADATA_HOST',
] as const;

// As pkg/hubprofile ValidateName (profile names and run labels).
const NAME_RE = /^[a-z0-9][a-z0-9_-]{0,62}$/;
const EMAIL_RE = /^[^\s@]+@[^\s@]+$/;

const README = 'see web/e2e/iap-journey/README.md';

/**
 * Returns the journey's configuration, or the reason it must skip: the
 * opt-in IAP_JOURNEY=1 is not set, a required variable is missing, or a
 * value has the wrong form.
 */
export function iapJourneyConfig(
  env: NodeJS.ProcessEnv,
  now: () => number = Date.now
): { config: IapJourneyConfig } | { skip: string } {
  if (env.IAP_JOURNEY !== '1') {
    return {
      skip: `IAP browser journey: operator-run only; set IAP_JOURNEY=1 and its environment (${README})`,
    };
  }
  const missing = IAP_JOURNEY_REQUIRED.filter((k) => !(env[k] ?? '').trim());
  if (missing.length > 0) {
    return { skip: `IAP browser journey: missing ${missing.join(', ')} (${README})` };
  }
  if (env.SCION_METADATA_MODE !== 'assign') {
    return {
      skip: 'IAP browser journey: the metadata emulator must be in assign mode (SCION_METADATA_MODE=assign)',
    };
  }
  const profile = (env.IAP_JOURNEY_PROFILE ?? '').trim();
  const run = (env.IAP_JOURNEY_RUN ?? '').trim() || `iapj-${now().toString(36)}`;
  if (!NAME_RE.test(profile) || !NAME_RE.test(run)) {
    return {
      skip: 'IAP browser journey: IAP_JOURNEY_PROFILE and IAP_JOURNEY_RUN must be 1-63 lowercase letters, digits, - or _',
    };
  }
  return { config: { profile, run, scionBin: (env.SCION_BIN ?? '').trim() || 'scion' } };
}

/**
 * Reads the journey's target from the JSON of `scion hub profile show
 * <name> --json`. Throws (with no value from the profile in the message)
 * when the profile is not an IAP profile for an https origin.
 */
export function profileTarget(showJSON: string): IapJourneyTarget {
  type ProfileView = {
    endpoint?: unknown;
    transport?: unknown;
    iapAudience?: unknown;
    uatServiceAccount?: unknown;
  };
  let p: ProfileView;
  try {
    p = (JSON.parse(showJSON) as { profile?: ProfileView }).profile ?? {};
  } catch {
    throw new Error('profileTarget: the profile output is not JSON');
  }
  if (p.transport !== 'iap') {
    throw new Error('profileTarget: the profile must use transport iap');
  }
  let hub: URL;
  try {
    hub = new URL(typeof p.endpoint === 'string' ? p.endpoint : '');
  } catch {
    throw new Error('profileTarget: the profile endpoint is not a URL');
  }
  if (hub.protocol !== 'https:' || (hub.pathname !== '/' && hub.pathname !== '')) {
    throw new Error('profileTarget: the profile endpoint must be an https origin with no path');
  }
  const sa = typeof p.uatServiceAccount === 'string' ? p.uatServiceAccount.trim() : '';
  const aud = typeof p.iapAudience === 'string' ? p.iapAudience.trim() : '';
  if (!EMAIL_RE.test(sa) || !aud) {
    throw new Error('profileTarget: the profile needs its IAP service account and IAP audience');
  }
  return { hubURL: hub.origin, driverServiceAccount: sa, iapAudience: aud };
}
