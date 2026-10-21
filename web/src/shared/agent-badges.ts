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
 * Agent badges (ptone/scion#5503): what the harness vendor mark and the
 * model size chip show for an agent. Pure data and functions, no DOM; the
 * `scion-agent-badges` element renders the result.
 *
 * The hub derives both inputs (ptone/scion#5502): `harnessVendor` from a
 * fixed harness map (pkg/harness Vendor) and `modelSize` from the size alias
 * the agent was created with. The web does not re-derive either.
 */

import type { Agent } from './types.js';
import { harnessDisplayName } from './harness-utils.js';

/**
 * How one harness vendor is drawn. Today every mark is a neutral coloured
 * monogram. To switch a vendor to a logo, set `icon` to a registered
 * `sl-icon` name (and add it to USED_ICONS in
 * scripts/copy-shoelace-icons.mjs): the element draws the icon instead of
 * the monogram, and nothing else changes. Vendor logos need a brand-usage
 * check before they ship.
 */
export interface VendorMark {
  /** Display name, used in the tooltip. */
  label: string;
  /** One or two characters drawn in the mark. */
  monogram: string;
  /** Mark colour (a CSS colour; Shoelace tokens with fallbacks). */
  color: string;
  /**
   * True for a multi-vendor harness (the vendor is the harness itself): the
   * mark is drawn outlined and no size chip is shown.
   */
  multiVendor?: boolean;
  /** Optional sl-icon name drawn instead of the monogram. */
  icon?: string;
}

/**
 * The swappable vendor map: hub `harnessVendor` value to its mark. Keys are
 * the values pkg/harness Vendor returns. Multi-vendor harnesses (opencode,
 * copilot, hermes) map to their own harness identity there, so they get the
 * harness initial here.
 */
export const VENDOR_MARKS: Readonly<Record<string, VendorMark>> = {
  anthropic: { label: 'Anthropic', monogram: 'A', color: 'var(--sl-color-orange-600, #ea580c)' },
  google: { label: 'Google', monogram: 'G', color: 'var(--sl-color-blue-600, #2563eb)' },
  openai: { label: 'OpenAI', monogram: 'O', color: 'var(--sl-color-emerald-600, #059669)' },
  xai: { label: 'xAI', monogram: 'x', color: 'var(--sl-color-neutral-800, #27272a)' },
  meta: { label: 'Meta', monogram: 'M', color: 'var(--sl-color-indigo-600, #4f46e5)' },
  opencode: {
    label: 'OpenCode',
    monogram: 'O',
    color: 'var(--sl-color-neutral-600, #52525b)',
    multiVendor: true,
  },
  copilot: {
    label: 'Copilot',
    monogram: 'C',
    color: 'var(--sl-color-neutral-600, #52525b)',
    multiVendor: true,
  },
  hermes: {
    label: 'Hermes',
    monogram: 'H',
    color: 'var(--sl-color-neutral-600, #52525b)',
    multiVendor: true,
  },
};

/** The mark of a custom or unresolved harness (no `harnessVendor`). */
export const UNKNOWN_VENDOR_MARK: VendorMark = {
  label: 'Unknown',
  monogram: '?',
  color: 'var(--sl-color-neutral-400, #a1a1aa)',
};

/** Model size alias to its chip text and its full word for the tooltip. */
export const MODEL_SIZE_CHIPS: Readonly<Record<string, { chip: string; label: string }>> = {
  small: { chip: 'S', label: 'Small' },
  medium: { chip: 'M', label: 'Medium' },
  large: { chip: 'L', label: 'Large' },
  'extra-large': { chip: 'XL', label: 'Extra large' },
};

/** What the badges show for one agent. */
export interface AgentBadgeView {
  mark: VendorMark;
  /** The size chip, or null when there is none to show. */
  size: { chip: string; label: string } | null;
  /** The tooltip and accessible label. */
  title: string;
}

/** The mark for a hub `harnessVendor` value (case-insensitive). */
export function vendorMark(vendor: string | undefined): VendorMark {
  if (!vendor) return UNKNOWN_VENDOR_MARK;
  return VENDOR_MARKS[vendor.toLowerCase()] ?? UNKNOWN_VENDOR_MARK;
}

/**
 * What the badges show: the vendor mark, and a size chip only for a known
 * size on a harness that is not multi-vendor. `harness` is the harness name
 * when the row has it (full rows: `appliedConfig.harness` or
 * `resolvedHarness`; compact rows carry only the vendor).
 */
export function agentBadgeView(
  vendor: string | undefined,
  size: string | undefined,
  harness?: string
): AgentBadgeView {
  const mark = vendorMark(vendor);
  const chip = !mark.multiVendor && size ? (MODEL_SIZE_CHIPS[size] ?? null) : null;
  const harnessText = harness
    ? `Harness: ${harnessDisplayName(harness)}${mark === UNKNOWN_VENDOR_MARK || mark.multiVendor ? '' : ` (${mark.label})`}`
    : mark === UNKNOWN_VENDOR_MARK
      ? 'Harness: unknown'
      : mark.multiVendor
        ? `Harness: ${mark.label}`
        : `Harness vendor: ${mark.label}`;
  const title = chip ? `${harnessText}\nConfigured model size: ${chip.label}` : harnessText;
  return { mark, size: chip, title };
}

/** The harness name a row carries, if any (absent on compact rows). */
export function agentHarnessName(agent: Agent): string | undefined {
  return agent.appliedConfig?.harness || agent.resolvedHarness || undefined;
}
