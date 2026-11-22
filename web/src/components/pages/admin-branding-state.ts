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
 * Pure state, diffing, validation, and data: URI budget helpers for the
 * Admin Branding page (/admin/branding).
 */

export const BRANDING_KOANF_PREFIX = 'server.hub.branding';

export const MAX_BRAND_NAME_RUNES = 64;
export const MAX_BRAND_SHORT_NAME_RUNES = 24;
export const MAX_BRAND_SUBTITLE_RUNES = 80;
export const MAX_BRAND_FONT_SANS_LEN = 64;
export const MAX_BRAND_HTTPS_URL_LEN = 2048;
export const MAX_BRAND_PATH_LEN = 512;

/** Maximum decoded bytes per embedded data: image asset (128 KiB). */
export const MAX_BRAND_ASSET_BYTES = 128 * 1024;
/** Maximum total decoded bytes across all embedded data: assets (512 KiB). */
export const MAX_BRAND_TOTAL_BYTES = 512 * 1024;

/** Stock Scion primary fallback when --scion-primary is unavailable. */
export const DEFAULT_ACCENT_PICKER_COLOR = `#${'3b82f6'}`;

export const GOOGLE_FONT_FAMILY_RE = /^[A-Za-z0-9]([A-Za-z0-9 -]{0,62}[A-Za-z0-9-])?$/;
export const HEX_COLOR_RE = /^#[0-9a-f]{6}$/i;

export const CUSTOM_FONT_SENTINEL = '__custom__';

export interface FontSansOption {
  value: string;
  label: string;
}

export const FONT_SANS_OPTIONS: readonly FontSansOption[] = [
  { value: 'default', label: 'Default (Inter)' },
  { value: 'system', label: 'System OS Sans (no CDN)' },
  { value: 'ibm-plex-sans', label: 'IBM Plex Sans' },
  { value: 'plus-jakarta-sans', label: 'Plus Jakarta Sans' },
  { value: 'dm-sans', label: 'DM Sans' },
  { value: 'source-sans-3', label: 'Source Sans 3' },
];

const CURATED_FONT_VALUES = new Set(FONT_SANS_OPTIONS.map((o) => o.value));

export type BrandAttribution = 'shown' | 'hidden';
export type BrandCorners = 'default' | 'square' | 'rounded';

export type LogoSlotKey = 'light' | 'dark';
export type IconSlotKey =
  | 'favicon'
  | 'apple_touch_icon'
  | 'icon_192'
  | 'icon_512'
  | 'icon_maskable_512';

export type AssetSlotPath = `logo.${LogoSlotKey}` | `icons.${IconSlotKey}`;

export interface AssetSlotDef {
  key: AssetSlotPath;
  group: 'logo' | 'icons';
  field: LogoSlotKey | IconSlotKey;
  label: string;
  accept: string;
  allowedMimes: readonly string[];
  allowedExtensions: readonly string[];
  pngOnly: boolean;
  hint: string;
  dimensionHint?: string;
}

export const ASSET_SLOTS: readonly AssetSlotDef[] = [
  {
    key: 'logo.light',
    group: 'logo',
    field: 'light',
    label: 'Light Logo',
    accept: '.svg,.png,.webp',
    allowedMimes: ['image/svg+xml', 'image/png', 'image/webp'],
    allowedExtensions: ['.svg', '.png', '.webp'],
    pngOnly: false,
    hint: 'Displayed on light backgrounds (SVG, PNG, or WebP).',
  },
  {
    key: 'logo.dark',
    group: 'logo',
    field: 'dark',
    label: 'Dark Logo',
    accept: '.svg,.png,.webp',
    allowedMimes: ['image/svg+xml', 'image/png', 'image/webp'],
    allowedExtensions: ['.svg', '.png', '.webp'],
    pngOnly: false,
    hint: 'Displayed on dark backgrounds. Falls back to Light Logo when unset.',
  },
  {
    key: 'icons.favicon',
    group: 'icons',
    field: 'favicon',
    label: 'Favicon',
    accept: '.svg,.png,.ico,.webp',
    allowedMimes: [
      'image/svg+xml',
      'image/png',
      'image/x-icon',
      'image/vnd.microsoft.icon',
      'image/webp',
    ],
    allowedExtensions: ['.svg', '.png', '.ico', '.webp'],
    pngOnly: false,
    hint: 'Browser tab icon (SVG, PNG, ICO, or WebP).',
    dimensionHint: '32×32 or SVG',
  },
  {
    key: 'icons.apple_touch_icon',
    group: 'icons',
    field: 'apple_touch_icon',
    label: 'Apple Touch Icon',
    accept: '.png',
    allowedMimes: ['image/png'],
    allowedExtensions: ['.png'],
    pngOnly: true,
    hint: 'iOS home screen icon (PNG only).',
    dimensionHint: '180×180 PNG',
  },
  {
    key: 'icons.icon_192',
    group: 'icons',
    field: 'icon_192',
    label: 'PWA Icon (192×192)',
    accept: '.png',
    allowedMimes: ['image/png'],
    allowedExtensions: ['.png'],
    pngOnly: true,
    hint: 'Web app manifest icon (PNG only).',
    dimensionHint: '192×192 PNG',
  },
  {
    key: 'icons.icon_512',
    group: 'icons',
    field: 'icon_512',
    label: 'PWA Icon (512×512)',
    accept: '.png',
    allowedMimes: ['image/png'],
    allowedExtensions: ['.png'],
    pngOnly: true,
    hint: 'High-resolution web app manifest icon (PNG only).',
    dimensionHint: '512×512 PNG',
  },
  {
    key: 'icons.icon_maskable_512',
    group: 'icons',
    field: 'icon_maskable_512',
    label: 'Maskable PWA Icon (512×512)',
    accept: '.png',
    allowedMimes: ['image/png'],
    allowedExtensions: ['.png'],
    pngOnly: true,
    hint: 'Adaptive launcher icon with safe-zone padding (PNG only).',
    dimensionHint: '512×512 PNG',
  },
];

export interface RawBrandingConfig {
  name?: string;
  short_name?: string;
  subtitle?: string;
  attribution?: string;
  corners?: string;
  font_sans?: string;
  accent_color?: string;
  logo?: {
    light?: string;
    dark?: string;
  };
  icons?: {
    favicon?: string;
    apple_touch_icon?: string;
    icon_192?: string;
    icon_512?: string;
    icon_maskable_512?: string;
  };
  palette?: Record<string, unknown>;
}

export interface BrandingFormState {
  name: string;
  short_name: string;
  subtitle: string;
  attribution: BrandAttribution;
  corners: BrandCorners;
  font_sans: string;
  accent_color: string;
  logo: Record<LogoSlotKey, string>;
  icons: Record<IconSlotKey, string>;
}

export function defaultBrandingFormState(): BrandingFormState {
  return {
    name: '',
    short_name: '',
    subtitle: '',
    attribution: 'shown',
    corners: 'default',
    font_sans: 'default',
    accent_color: '',
    logo: {
      light: '',
      dark: '',
    },
    icons: {
      favicon: '',
      apple_touch_icon: '',
      icon_192: '',
      icon_512: '',
      icon_maskable_512: '',
    },
  };
}

export function cloneBrandingFormState(s: BrandingFormState): BrandingFormState {
  return {
    ...s,
    logo: { ...s.logo },
    icons: { ...s.icons },
  };
}

export function brandingFromConfig(raw: RawBrandingConfig | null | undefined): BrandingFormState {
  const base = defaultBrandingFormState();
  if (!raw) return base;
  return {
    name: raw.name ?? '',
    short_name: raw.short_name ?? '',
    subtitle: raw.subtitle ?? '',
    attribution: raw.attribution === 'hidden' ? 'hidden' : 'shown',
    corners: raw.corners === 'square' || raw.corners === 'rounded' ? raw.corners : 'default',
    font_sans: raw.font_sans && raw.font_sans.trim() !== '' ? raw.font_sans : 'default',
    accent_color: raw.accent_color ?? '',
    logo: {
      light: raw.logo?.light ?? '',
      dark: raw.logo?.dark ?? '',
    },
    icons: {
      favicon: raw.icons?.favicon ?? '',
      apple_touch_icon: raw.icons?.apple_touch_icon ?? '',
      icon_192: raw.icons?.icon_192 ?? '',
      icon_512: raw.icons?.icon_512 ?? '',
      icon_maskable_512: raw.icons?.icon_maskable_512 ?? '',
    },
  };
}

export function hasAdvancedPaletteOverrides(raw: RawBrandingConfig | null | undefined): boolean {
  const palette = raw?.palette;
  if (!palette || typeof palette !== 'object') return false;
  for (const val of Object.values(palette)) {
    if (val && typeof val === 'object' && !Array.isArray(val)) {
      if (
        Object.values(val as Record<string, unknown>).some((v) => typeof v === 'string' && v !== '')
      ) {
        return true;
      }
    }
  }
  return false;
}

export function hasAnyConfiguredBranding(state: BrandingFormState, hasPalette: boolean): boolean {
  if (hasPalette) return true;
  return (
    state.name !== '' ||
    state.short_name !== '' ||
    state.subtitle !== '' ||
    state.attribution !== 'shown' ||
    state.corners !== 'default' ||
    (state.font_sans !== '' && state.font_sans !== 'default') ||
    state.accent_color !== '' ||
    Object.values(state.logo).some((v) => v !== '') ||
    Object.values(state.icons).some((v) => v !== '')
  );
}

export function isCuratedOrBuiltinFont(value: string): boolean {
  return value === '' || CURATED_FONT_VALUES.has(value);
}

export function getSlotValue(state: BrandingFormState, slot: AssetSlotDef): string {
  if (slot.group === 'logo') return state.logo[slot.field as LogoSlotKey];
  return state.icons[slot.field as IconSlotKey];
}

export function setSlotValue(
  state: BrandingFormState,
  slot: AssetSlotDef,
  value: string
): BrandingFormState {
  if (slot.group === 'logo') {
    return {
      ...state,
      logo: { ...state.logo, [slot.field]: value },
    };
  }
  return {
    ...state,
    icons: { ...state.icons, [slot.field]: value },
  };
}

/**
 * Builds a minimal dirty-diff object for `PUT /api/v1/admin/server-config`
 * under `server.hub.branding`.
 *
 * - Omits unchanged keys so untouched CLI `palette.*` overrides and large
 *   unchanged `data:` assets are preserved via backend deep-merge.
 * - Omits any key in `envLockedKeys` (full koanf paths).
 * - Emits `""` when a field or enum is reset back to its stock default so
 *   deep-merge removes the stored key.
 */
export function buildBrandingDiff(
  draft: BrandingFormState,
  baseline: BrandingFormState,
  envLockedKeys: ReadonlySet<string>
): Record<string, unknown> {
  const diff: Record<string, unknown> = {};
  const isLocked = (relPath: string): boolean =>
    envLockedKeys.has(`${BRANDING_KOANF_PREFIX}.${relPath}`);

  const topStrings: readonly ('name' | 'short_name' | 'subtitle' | 'accent_color')[] = [
    'name',
    'short_name',
    'subtitle',
    'accent_color',
  ];
  for (const key of topStrings) {
    const nextVal = key === 'accent_color' ? draft[key].trim() : draft[key];
    const baseVal = key === 'accent_color' ? baseline[key].trim() : baseline[key];
    if (!isLocked(key) && nextVal !== baseVal) {
      diff[key] = nextVal;
    }
  }

  if (!isLocked('attribution') && draft.attribution !== baseline.attribution) {
    diff.attribution = draft.attribution === 'shown' ? '' : draft.attribution;
  }

  if (!isLocked('corners') && draft.corners !== baseline.corners) {
    diff.corners = draft.corners === 'default' ? '' : draft.corners;
  }

  if (!isLocked('font_sans') && draft.font_sans !== baseline.font_sans) {
    diff.font_sans = draft.font_sans === 'default' ? '' : draft.font_sans;
  }

  const logoDiff: Record<string, string> = {};
  for (const key of ['light', 'dark'] as const) {
    if (!isLocked(`logo.${key}`) && draft.logo[key] !== baseline.logo[key]) {
      logoDiff[key] = draft.logo[key];
    }
  }
  if (Object.keys(logoDiff).length > 0) {
    diff.logo = logoDiff;
  }

  const iconsDiff: Record<string, string> = {};
  for (const key of [
    'favicon',
    'apple_touch_icon',
    'icon_192',
    'icon_512',
    'icon_maskable_512',
  ] as const) {
    if (!isLocked(`icons.${key}`) && draft.icons[key] !== baseline.icons[key]) {
      iconsDiff[key] = draft.icons[key];
    }
  }
  if (Object.keys(iconsDiff).length > 0) {
    diff.icons = iconsDiff;
  }

  return diff;
}

export function isBrandingDirty(
  draft: BrandingFormState,
  baseline: BrandingFormState,
  envLockedKeys: ReadonlySet<string>
): boolean {
  return Object.keys(buildBrandingDiff(draft, baseline, envLockedKeys)).length > 0;
}

/**
 * Builds an explicit clear-all `branding` payload for file-backed hubs
 * (`settings_tier === 'file'`), where `DELETE /api/v1/admin/server-config/sections/branding`
 * returns 400. Leaves any env-locked key untouched.
 */
export function buildFileModeResetPayload(
  envLockedKeys: ReadonlySet<string>,
  hasPalette: boolean
): Record<string, unknown> {
  const payload: Record<string, unknown> = {};
  const isLocked = (relPath: string): boolean =>
    envLockedKeys.has(`${BRANDING_KOANF_PREFIX}.${relPath}`);

  for (const key of [
    'name',
    'short_name',
    'subtitle',
    'attribution',
    'corners',
    'font_sans',
    'accent_color',
  ] as const) {
    if (!isLocked(key)) {
      payload[key] = '';
    }
  }

  const logo: Record<string, string> = {};
  for (const key of ['light', 'dark'] as const) {
    if (!isLocked(`logo.${key}`)) logo[key] = '';
  }
  if (Object.keys(logo).length > 0) payload.logo = logo;

  const icons: Record<string, string> = {};
  for (const key of [
    'favicon',
    'apple_touch_icon',
    'icon_192',
    'icon_512',
    'icon_maskable_512',
  ] as const) {
    if (!isLocked(`icons.${key}`)) icons[key] = '';
  }
  if (Object.keys(icons).length > 0) payload.icons = icons;

  const anyPaletteLocked = Array.from(envLockedKeys).some((k) =>
    k.startsWith(`${BRANDING_KOANF_PREFIX}.palette`)
  );
  if (hasPalette && !anyPaletteLocked) {
    payload.palette = null;
  }

  return payload;
}

export interface DataUriInfo {
  mediaType: string;
  decodedBytes: number;
}

/**
 * Parses a base64 `data:image/...;base64,...` URI and computes its exact
 * decoded byte count without decoding the payload into memory.
 */
export function parseDataUriInfo(value: string): DataUriInfo | null {
  if (!value || !value.toLowerCase().startsWith('data:')) return null;
  const commaIdx = value.indexOf(',');
  if (commaIdx < 0) return null;
  const header = value.slice(5, commaIdx).toLowerCase();
  const payload = value.slice(commaIdx + 1);
  if (!header.endsWith(';base64')) return null;
  let mediaType = header.slice(0, -';base64'.length);
  if (mediaType === 'image/vnd.microsoft.icon') {
    mediaType = 'image/x-icon';
  }
  const cleanLen = payload.replace(/\s+/g, '').length;
  let padding = 0;
  if (payload.endsWith('==')) padding = 2;
  else if (payload.endsWith('=')) padding = 1;
  const decodedBytes = Math.max(0, Math.floor((cleanLen * 3) / 4) - padding);
  return { mediaType, decodedBytes };
}

export function totalDataUriBytes(state: BrandingFormState): number {
  let total = 0;
  for (const slot of ASSET_SLOTS) {
    const info = parseDataUriInfo(getSlotValue(state, slot));
    if (info) total += info.decodedBytes;
  }
  return total;
}

export function formatKiB(bytes: number): string {
  return `${(bytes / 1024).toFixed(1)} KiB`;
}

export function validateAssetFile(
  file: { name: string; type: string; size: number },
  slot: AssetSlotDef,
  otherSlotsDataBytes: number
): string | null {
  const lowerName = file.name.toLowerCase();
  const mime = file.type.toLowerCase();
  const extOk = slot.allowedExtensions.some((ext) => lowerName.endsWith(ext));
  const mimeOk = mime !== '' && slot.allowedMimes.includes(mime);

  if (slot.pngOnly) {
    if (mime !== '' && mime !== 'image/png') {
      return `${slot.label} must be a PNG image (.png), got ${mime}.`;
    }
    if (!mimeOk && !extOk) {
      return `${slot.label} must be a PNG image (.png).`;
    }
  } else {
    if (mime !== '' && !mimeOk) {
      return `Unsupported file type ${mime} for ${slot.label}. Allowed: ${slot.allowedExtensions.join(', ')}.`;
    }
    if (!mimeOk && !extOk) {
      return `Unsupported file format for ${slot.label}. Allowed: ${slot.allowedExtensions.join(', ')}.`;
    }
  }

  if (file.size === 0) {
    return 'Selected file is empty.';
  }

  if (file.size > MAX_BRAND_ASSET_BYTES) {
    return `File is ${formatKiB(file.size)}, which exceeds the 128 KiB per-asset limit.`;
  }

  if (otherSlotsDataBytes + file.size > MAX_BRAND_TOTAL_BYTES) {
    const remaining = Math.max(0, MAX_BRAND_TOTAL_BYTES - otherSlotsDataBytes);
    return `Adding ${formatKiB(file.size)} exceeds the 512 KiB total embedded asset budget (${formatKiB(remaining)} remaining).`;
  }

  return null;
}

const EXT_TO_MIME: Readonly<Record<string, string>> = {
  '.svg': 'image/svg+xml',
  '.png': 'image/png',
  '.webp': 'image/webp',
  '.ico': 'image/x-icon',
};

/**
 * Rewrites generic `data:application/octet-stream;base64,...` or empty-MIME
 * `data:;base64,...` headers produced by FileReader when the OS/browser omits
 * `file.type`, using the allowed extension on `fileName`.
 */
export function normalizeAssetDataUrl(
  dataUrl: string,
  fileName: string,
  slot: AssetSlotDef
): string {
  const lower = dataUrl.toLowerCase();
  const genericPrefix = lower.startsWith('data:application/octet-stream;base64,')
    ? 'data:application/octet-stream;base64,'
    : lower.startsWith('data:;base64,')
      ? 'data:;base64,'
      : null;
  if (!genericPrefix) return dataUrl;
  const lowerName = fileName.toLowerCase();
  for (const ext of slot.allowedExtensions) {
    if (lowerName.endsWith(ext) && EXT_TO_MIME[ext]) {
      return `data:${EXT_TO_MIME[ext]};base64,${dataUrl.slice(genericPrefix.length)}`;
    }
  }
  return dataUrl;
}

export function validateAssetUrlInput(raw: string): string | null {
  if (raw === '') return null;
  if (raw.toLowerCase().startsWith('data:')) return null;
  if (/\s/.test(raw)) {
    return 'Asset URI must not contain whitespace.';
  }
  if (/^http:\/\//i.test(raw)) {
    return 'Plain http:// URLs are blocked by Hub Content-Security-Policy (img-src); use https:// or a same-origin /path.';
  }
  if (raw.startsWith('//')) {
    return 'Protocol-relative // paths are not allowed; use https:// or a single-slash /path.';
  }
  if (raw.startsWith('/')) {
    if (raw.length > MAX_BRAND_PATH_LEN) {
      return `Same-origin path must be at most ${MAX_BRAND_PATH_LEN} characters.`;
    }
    return null;
  }
  if (/^https:\/\//i.test(raw)) {
    if (raw.length > MAX_BRAND_HTTPS_URL_LEN) {
      return `URL must be at most ${MAX_BRAND_HTTPS_URL_LEN} characters.`;
    }
    return null;
  }
  return 'Must start with https:// or / (same-origin path).';
}

export function validateFontSansInput(raw: string): string | null {
  if (raw === '' || isCuratedOrBuiltinFont(raw)) return null;
  if (raw.length > MAX_BRAND_FONT_SANS_LEN || !GOOGLE_FONT_FAMILY_RE.test(raw)) {
    return `Must be a valid Google Fonts family name (1–${MAX_BRAND_FONT_SANS_LEN} ASCII letters, digits, internal spaces, or hyphens).`;
  }
  return null;
}

export function validateAccentColorInput(raw: string): string | null {
  if (raw === '') return null;
  if (!HEX_COLOR_RE.test(raw.trim())) {
    return 'Must be a 6-digit hex color in #RRGGBB format.';
  }
  return null;
}
