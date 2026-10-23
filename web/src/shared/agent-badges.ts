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
 * The vendor mark images: 32x32 PNGs (16 CSS px at 2x) under
 * src/assets/vendor-marks/, named after the hub `harnessVendor` value.
 * Today each is a neutral monogram (a coloured rounded square with an
 * initial). Switching a vendor to its branded logo is a file replacement
 * only: overwrite `<vendor>.png` with a 32x32 PNG of the same name (a
 * transparent background is fine; keep it small, about 1 KB). Vendor logos
 * need a brand-usage check before they ship.
 *
 * `?no-inline` keeps Vite from inlining these small files as base64 data
 * URIs: each one is emitted as its own content-hashed asset, so it gets the
 * immutable cache headers and adds only its URL to the JS.
 */
import anthropicMark from '../assets/vendor-marks/anthropic.png?no-inline';
import googleMark from '../assets/vendor-marks/google.png?no-inline';
import openaiMark from '../assets/vendor-marks/openai.png?no-inline';
import xaiMark from '../assets/vendor-marks/xai.png?no-inline';
import metaMark from '../assets/vendor-marks/meta.png?no-inline';
import opencodeMark from '../assets/vendor-marks/opencode.png?no-inline';
import copilotMark from '../assets/vendor-marks/copilot.png?no-inline';
import hermesMark from '../assets/vendor-marks/hermes.png?no-inline';
import unknownMark from '../assets/vendor-marks/unknown.png?no-inline';

/** How one harness vendor is drawn. */
export interface VendorMark {
  /** Display name, used as the image's alt text and in the tooltip. */
  label: string;
  /** URL of the mark image (a Vite-emitted, content-hashed asset). */
  src: string;
  /**
   * True for a multi-vendor harness (the vendor is the harness itself): no
   * size chip is shown.
   */
  multiVendor?: boolean;
}

/**
 * The swappable vendor map: hub `harnessVendor` value to its mark. Keys are
 * the values pkg/harness Vendor returns. Multi-vendor harnesses (opencode,
 * copilot, hermes) map to their own harness identity there.
 */
export const VENDOR_MARKS: Readonly<Record<string, VendorMark>> = {
  anthropic: { label: 'Anthropic', src: anthropicMark },
  google: { label: 'Google', src: googleMark },
  openai: { label: 'OpenAI', src: openaiMark },
  xai: { label: 'xAI', src: xaiMark },
  meta: { label: 'Meta', src: metaMark },
  opencode: { label: 'OpenCode', src: opencodeMark, multiVendor: true },
  copilot: { label: 'Copilot', src: copilotMark, multiVendor: true },
  hermes: { label: 'Hermes', src: hermesMark, multiVendor: true },
};

/** The mark of a custom or unresolved harness (no `harnessVendor`). */
export const UNKNOWN_VENDOR_MARK: VendorMark = { label: 'Unknown', src: unknownMark };

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
  // A vendor the hub knows but this map lacks: the neutral image, labelled
  // with the hub's own vendor string so the tooltip still says whose it is.
  return VENDOR_MARKS[vendor.toLowerCase()] ?? { label: vendor, src: UNKNOWN_VENDOR_MARK.src };
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
  const chip = !mark.multiVendor && size ? (MODEL_SIZE_CHIPS[size.toLowerCase()] ?? null) : null;
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
