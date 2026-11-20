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
 * Lightweight Hub branding state & event bus.
 *
 * Kept small because it sits in the static entry closure (main.js / nav /
 * header). DOM mutation for custom palette CSS, favicons and theme-color is
 * lazy-loaded from ./branding-runtime.js only when custom branding is active
 * or being cleared.
 */

import type { PublicBranding } from '../shared/types.js';
import { DEFAULT_APP_NAME, setAppName } from './page-title.js';

export const BRANDING_STYLE_ID = 'scion-branding-tokens';
export const BRANDING_UPDATED_EVENT = 'scion:branding-updated';
export const DEFAULT_NOTIFICATION_ICON = '/scion-notification-icon.png';
export const DEFAULT_BRAND_SUBTITLE = 'Agent Orchestration Platform';

let currentBranding: Readonly<PublicBranding> | null = null;
let currentBrandingKey = '';
let hadCustomDom = false;
let runtimeSyncSeq = 0;

/** Returns the active Hub branding payload, or null when using stock defaults. */
export function getBranding(): Readonly<PublicBranding> | null {
  return currentBranding;
}

/** Returns the active brand display name ('Scion' when uncustomized). */
export function getBrandName(): string {
  const name = currentBranding?.name?.trim();
  return name && name.length > 0 ? name : DEFAULT_APP_NAME;
}

/**
 * Reports whether custom brand identity (custom name or custom logo) is active.
 * Governs "Powered by Scion" attribution and compact monogram fallback.
 */
export function isCustomBranding(): boolean {
  if (!currentBranding) return false;
  return (
    getBrandName() !== DEFAULT_APP_NAME ||
    Boolean(currentBranding.logoLight) ||
    Boolean(currentBranding.logoDark)
  );
}

/**
 * Reports whether "Powered by Scion" attribution should be rendered on branded
 * chrome surfaces (/login, /invite, expanded sidebar footers).
 */
export function shouldShowAttribution(): boolean {
  return isCustomBranding() && currentBranding?.attribution !== 'hidden';
}

/**
 * Resolves the subtitle for login/sidebar surfaces:
 * - explicit branding.subtitle when set
 * - hidden ('') when brand name is customized and subtitle is omitted
 * - stock fallback when unbranded
 */
export function getBrandSubtitle(stockFallback = DEFAULT_BRAND_SUBTITLE): string {
  const custom = currentBranding?.subtitle?.trim();
  if (custom) return custom;
  if (getBrandName() !== DEFAULT_APP_NAME) return '';
  return stockFallback;
}

/** Returns the first grapheme of the active brand name for compact 64px rail badges. */
export function getBrandMonogram(): string {
  const first = Array.from(getBrandName())[0];
  return first ? first.toUpperCase() : 'S';
}

/** Returns the desktop notification icon URL, falling back to stock Scion. */
export function getNotificationIconUrl(): string {
  return currentBranding?.notificationIconUrl || DEFAULT_NOTIFICATION_ICON;
}

/**
 * Applies a Hub branding payload (from __SCION_DATA__.branding or
 * GET /api/v1/settings/public). Passing null/undefined resets to stock Scion.
 */
export function applyBranding(branding?: PublicBranding | null): void {
  const next = branding ?? null;
  const nextKey = next ? JSON.stringify(next) : '';
  const changed = nextKey !== currentBrandingKey;

  currentBranding = next;
  currentBrandingKey = nextKey;
  setAppName(next?.name);

  if (typeof document !== 'undefined') {
    const needsDomSync =
      next !== null || hadCustomDom || document.getElementById(BRANDING_STYLE_ID) !== null;
    if (needsDomSync) {
      hadCustomDom = next !== null;
      const seq = ++runtimeSyncSeq;
      void import('./branding-runtime.js').then((mod) => {
        if (seq === runtimeSyncSeq) {
          mod.syncBrandingDOM(next);
        }
      });
    }
  }

  if (changed && typeof window !== 'undefined') {
    window.dispatchEvent(
      new CustomEvent<Readonly<PublicBranding> | null>(BRANDING_UPDATED_EVENT, {
        detail: next,
      })
    );
  }
}
