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
 * Client-side OKLCH shade ramp generator and WCAG 2.1 AA contrast evaluator
 * for Hub Admin Branding (/admin/branding).
 *
 * Mirrors `pkg/config/branding_palette.go` (sRGB <-> Oklab/OKLCH luminance-
 * anchored ramp, IEC 61966-2-1 relative luminance, alpha-composited dark badge
 * contrast, and the full server-side WCAG 2.1 AA pair matrix) so the live
 * preview and contrast badges agree with server validation (`422`).
 *
 * Advisory only: server-side `ValidateBranding` remains the source of truth.
 */

export const BRANDING_KOANF_PREFIX = 'server.hub.branding';

export const WCAG_AA_TEXT_MIN = 4.5;
export const WCAG_AA_UI_MIN = 3.0;

/** Floating-point epsilon matching `pkg/config/branding_palette.go` (`ratio + 1e-9 < minRatio`). */
const CONTRAST_EPSILON = 1e-9;

export const SCALE_STEPS = [
  '50',
  '100',
  '200',
  '300',
  '400',
  '500',
  '600',
  '700',
  '800',
  '900',
  '950',
] as const;

export type ScaleStep = (typeof SCALE_STEPS)[number];

export const SCALE_NAMES = ['primary', 'neutral', 'success', 'warning', 'danger'] as const;
export type ScaleName = (typeof SCALE_NAMES)[number];

export type BrandingScaleMap = Record<ScaleStep, string>;
export type PartialBrandingScale = Partial<Record<string, string>>;

export interface BrandingSemanticTokens {
  bg: string;
  bg_subtle: string;
  surface: string;
  surface_raised: string;
  text: string;
  text_secondary: string;
  text_muted: string;
  border: string;
  primary: string;
  primary_hover: string;
  primary_active: string;
  primary_text: string;
}

export type PartialSemanticTokens = Partial<BrandingSemanticTokens>;

export interface RawPaletteConfig {
  primary?: PartialBrandingScale | null;
  neutral?: PartialBrandingScale | null;
  success?: PartialBrandingScale | null;
  warning?: PartialBrandingScale | null;
  danger?: PartialBrandingScale | null;
  light?: PartialSemanticTokens | null;
  dark?: PartialSemanticTokens | null;
}

export interface BrandingPaletteInput {
  accentColor?: string;
  palette?: RawPaletteConfig | null;
}

export interface ContrastCheckResult {
  id: string;
  path: string;
  mode: 'light' | 'dark';
  label: string;
  targetLabel: string;
  fgHex: string;
  bgHex: string;
  ratio: number;
  minRatio: number;
  pass: boolean;
  message: string;
}

export interface ContrastPairBadge {
  id: string;
  mode: 'light' | 'dark';
  label: string;
  fgHex: string;
  bgHex: string;
  ratio: number;
  minRatio: number;
  pass: boolean;
}

export interface ResolvedBrandingPalette {
  scales: Record<ScaleName, BrandingScaleMap>;
  light: BrandingSemanticTokens;
  dark: BrandingSemanticTokens;
  hasCustomPalette: boolean;
  /** Non-empty when an input hex or step key is syntactically invalid. */
  syntaxErrors: Array<{ path: string; message: string }>;
  /** Full server-parity WCAG 2.1 AA contrast matrix evaluated for customized tokens. */
  checks: ContrastCheckResult[];
  summary: {
    total: number;
    passed: number;
    failed: number;
    allPass: boolean;
  };
  /** Key primary contrast pairs for Light & Dark accent badges. */
  keyPairs: {
    light: ContrastPairBadge[];
    dark: ContrastPairBadge[];
  };
}

interface ShadeAnchor {
  readonly step: ScaleStep;
  readonly targetY: number;
  readonly chromaFactor: number;
}

/**
 * Calibrated WCAG relative luminance (Y) targets and OKLCH chroma factors
 * matching `oklchShadeAnchors` in `pkg/config/branding_palette.go`.
 */
export const OKLCH_SHADE_ANCHORS: readonly ShadeAnchor[] = [
  { step: '50', targetY: 0.93, chromaFactor: 0.12 },
  { step: '100', targetY: 0.84, chromaFactor: 0.24 },
  { step: '200', targetY: 0.69, chromaFactor: 0.48 },
  { step: '300', targetY: 0.5, chromaFactor: 0.75 },
  { step: '400', targetY: 0.34, chromaFactor: 0.92 },
  { step: '500', targetY: 0.21, chromaFactor: 1.0 },
  { step: '600', targetY: 0.13, chromaFactor: 0.95 },
  { step: '700', targetY: 0.085, chromaFactor: 0.85 },
  { step: '800', targetY: 0.055, chromaFactor: 0.72 },
  { step: '900', targetY: 0.035, chromaFactor: 0.58 },
  { step: '950', targetY: 0.018, chromaFactor: 0.45 },
];

/** Helper to construct canonical `#rrggbb` stock reference values without bare CSS hex literals. */
function h(digits: string): string {
  return `#${digits}`;
}

const WHITE_HEX = h('ffffff');

export function defaultPrimaryScale(): BrandingScaleMap {
  return {
    '50': h('eff6ff'),
    '100': h('dbeafe'),
    '200': h('bfdbfe'),
    '300': h('93c5fd'),
    '400': h('60a5fa'),
    '500': h('3b82f6'),
    '600': h('2563eb'),
    '700': h('1d4ed8'),
    '800': h('1e40af'),
    '900': h('1e3a8a'),
    '950': h('172554'),
  };
}

export function defaultNeutralScale(): BrandingScaleMap {
  return {
    '50': h('f8fafc'),
    '100': h('f1f5f9'),
    '200': h('e2e8f0'),
    '300': h('cbd5e1'),
    '400': h('94a3b8'),
    '500': h('64748b'),
    '600': h('475569'),
    '700': h('334155'),
    '800': h('1e293b'),
    '900': h('0f172a'),
    '950': h('0f172a'),
  };
}

export function defaultSuccessScale(): BrandingScaleMap {
  return {
    '50': h('f0fdf4'),
    '100': h('dcfce7'),
    '200': h('bbf7d0'),
    '300': h('86efac'),
    '400': h('4ade80'),
    '500': h('22c55e'),
    '600': h('16a34a'),
    '700': h('15803d'),
    '800': h('166534'),
    '900': h('14532d'),
    '950': h('052e16'),
  };
}

export function defaultWarningScale(): BrandingScaleMap {
  return {
    '50': h('fffbeb'),
    '100': h('fef3c7'),
    '200': h('fde68a'),
    '300': h('fcd34d'),
    '400': h('fbbf24'),
    '500': h('f59e0b'),
    '600': h('d97706'),
    '700': h('b45309'),
    '800': h('92400e'),
    '900': h('78350f'),
    '950': h('451a03'),
  };
}

export function defaultDangerScale(): BrandingScaleMap {
  return {
    '50': h('fef2f2'),
    '100': h('fee2e2'),
    '200': h('fecaca'),
    '300': h('fca5a5'),
    '400': h('f87171'),
    '500': h('ef4444'),
    '600': h('dc2626'),
    '700': h('b91c1c'),
    '800': h('991b1b'),
    '900': h('7f1d1d'),
    '950': h('450a0a'),
  };
}

export function defaultLightSemanticMode(): BrandingSemanticTokens {
  return {
    bg: h('f8fafc'),
    bg_subtle: h('f1f5f9'),
    surface: WHITE_HEX,
    surface_raised: WHITE_HEX,
    text: h('1e293b'),
    text_secondary: h('475569'),
    text_muted: h('64748b'),
    border: h('e2e8f0'),
    primary: h('3b82f6'),
    primary_hover: h('2563eb'),
    primary_active: h('1d4ed8'),
    primary_text: WHITE_HEX,
  };
}

export function defaultDarkSemanticMode(): BrandingSemanticTokens {
  return {
    bg: h('0f172a'),
    bg_subtle: h('1e293b'),
    surface: h('1e293b'),
    surface_raised: h('334155'),
    text: h('f1f5f9'),
    text_secondary: h('cbd5e1'),
    text_muted: h('94a3b8'),
    border: h('334155'),
    primary: h('60a5fa'),
    primary_hover: h('93c5fd'),
    primary_active: h('3b82f6'),
    primary_text: h('0f172a'),
  };
}

const HEX_COLOR_STRICT_RE = /^#[0-9a-fA-F]{6}$/;

export function isValidHexColor(s: string): boolean {
  return HEX_COLOR_STRICT_RE.test(s);
}

function normalizeHex(s: string): string {
  return s.trim().toLowerCase();
}

export function parseHexRGB(hexStr: string): [number, number, number] | null {
  if (!isValidHexColor(hexStr)) return null;
  const v = parseInt(hexStr.slice(1), 16);
  return [(v >> 16) & 0xff, (v >> 8) & 0xff, v & 0xff];
}

function formatHexRGB(r: number, g: number, b: number): string {
  const toHex = (n: number): string => Math.max(0, Math.min(255, n)).toString(16).padStart(2, '0');
  return `#${toHex(r)}${toHex(g)}${toHex(b)}`;
}

function srgbChannelToLinear(c: number): number {
  if (c <= 0.04045) return c / 12.92;
  return Math.pow((c + 0.055) / 1.055, 2.4);
}

function linearChannelToSRGB(c: number): number {
  if (c <= 0) return 0;
  if (c >= 1) return 1;
  if (c <= 0.0031308) return 12.92 * c;
  return 1.055 * Math.pow(c, 1.0 / 2.4) - 0.055;
}

function linearLuminance(rLin: number, gLin: number, bLin: number): number {
  return 0.2126 * rLin + 0.7152 * gLin + 0.0722 * bLin;
}

function rgbFloatLuminance(r255: number, g255: number, b255: number): number {
  return linearLuminance(
    srgbChannelToLinear(r255 / 255.0),
    srgbChannelToLinear(g255 / 255.0),
    srgbChannelToLinear(b255 / 255.0)
  );
}

/** IEC 61966-2-1 relative luminance Y in [0, 1] for a validated #RRGGBB hex string. */
export function relativeLuminance(hexStr: string): number | null {
  const rgb = parseHexRGB(hexStr);
  if (!rgb) return null;
  return rgbFloatLuminance(rgb[0], rgb[1], rgb[2]);
}

function contrastFromLuminance(y1: number, y2: number): number {
  const hi = Math.max(y1, y2);
  const lo = Math.min(y1, y2);
  return (hi + 0.05) / (lo + 0.05);
}

/** WCAG 2.1 contrast ratio in [1, 21] between two #RRGGBB hex colors. */
export function contrastRatio(hexA: string, hexB: string): number | null {
  const y1 = relativeLuminance(hexA);
  if (y1 === null) return null;
  const y2 = relativeLuminance(hexB);
  if (y2 === null) return null;
  return contrastFromLuminance(y1, y2);
}

/**
 * WCAG contrast ratio of `fgHex` against `tintHex` alpha-composited at `alpha`
 * in [0, 1] over `surfaceHex`.
 */
export function compositeAlphaContrast(
  fgHex: string,
  tintHex: string,
  alpha: number,
  surfaceHex: string
): number | null {
  const fgY = relativeLuminance(fgHex);
  if (fgY === null) return null;
  const tint = parseHexRGB(tintHex);
  const surf = parseHexRGB(surfaceHex);
  if (!tint || !surf) return null;
  const cr = tint[0] * alpha + surf[0] * (1.0 - alpha);
  const cg = tint[1] * alpha + surf[1] * (1.0 - alpha);
  const cb = tint[2] * alpha + surf[2] * (1.0 - alpha);
  const bgY = rgbFloatLuminance(cr, cg, cb);
  return contrastFromLuminance(fgY, bgY);
}

/** Alpha-composites `tintHex` at `alpha` over `surfaceHex` and returns `#rrggbb`. */
export function compositeAlphaHex(tintHex: string, alpha: number, surfaceHex: string): string {
  const tint = parseHexRGB(tintHex);
  const surf = parseHexRGB(surfaceHex);
  if (!tint || !surf) return surfaceHex;
  return formatHexRGB(
    Math.round(tint[0] * alpha + surf[0] * (1.0 - alpha)),
    Math.round(tint[1] * alpha + surf[1] * (1.0 - alpha)),
    Math.round(tint[2] * alpha + surf[2] * (1.0 - alpha))
  );
}

/** Formats a WCAG contrast ratio like `5.17:1`. */
export function formatContrastRatio(ratio: number): string {
  return `${ratio.toFixed(2)}:1`;
}

// --- sRGB <-> Oklab <-> OKLCH (Björn Ottosson's reference matrices) ---

function srgbToOKLCH(r: number, g: number, b: number): [number, number, number] {
  const lr = srgbChannelToLinear(r / 255.0);
  const lg = srgbChannelToLinear(g / 255.0);
  const lb = srgbChannelToLinear(b / 255.0);

  const l = 0.4122214708 * lr + 0.5363325363 * lg + 0.0514459929 * lb;
  const m = 0.2119034982 * lr + 0.6806995451 * lg + 0.1073969566 * lb;
  const s = 0.0883024619 * lr + 0.2817188376 * lg + 0.6299787005 * lb;

  const lRoot = Math.cbrt(l);
  const mRoot = Math.cbrt(m);
  const sRoot = Math.cbrt(s);

  const L = 0.2104542553 * lRoot + 0.793617785 * mRoot - 0.0040720468 * sRoot;
  const okA = 1.9779984951 * lRoot - 2.428592205 * mRoot + 0.4505937099 * sRoot;
  const okB = 0.0259040371 * lRoot + 0.7827717662 * mRoot - 0.808675766 * sRoot;

  const C = Math.hypot(okA, okB);
  const hue = C > 1e-7 ? Math.atan2(okB, okA) : 0;
  return [L, C, hue];
}

function oklchToLinearSRGB(L: number, C: number, hue: number): [number, number, number] {
  const okA = C * Math.cos(hue);
  const okB = C * Math.sin(hue);

  const lRoot = L + 0.3963377774 * okA + 0.2158037573 * okB;
  const mRoot = L - 0.1055613458 * okA - 0.0638541728 * okB;
  const sRoot = L - 0.0894841775 * okA - 1.291485548 * okB;

  const l = lRoot * lRoot * lRoot;
  const m = mRoot * mRoot * mRoot;
  const s = sRoot * sRoot * sRoot;

  const r = 4.0767416621 * l - 3.3077115913 * m + 0.2309699292 * s;
  const g = -1.2684380046 * l + 2.6097574011 * m - 0.3413193965 * s;
  const b = -0.0041960863 * l - 0.7034186147 * m + 1.707614701 * s;
  return [r, g, b];
}

function inSRGBGamut(r: number, g: number, b: number): boolean {
  const eps = 1e-7;
  return r >= -eps && r <= 1 + eps && g >= -eps && g <= 1 + eps && b >= -eps && b <= 1 + eps;
}

function clamp01(v: number): number {
  return Math.min(1, Math.max(0, v));
}

function clampGamutOKLCH(L: number, wantC: number, hue: number): [number, number, number] {
  if (wantC <= 0) {
    const [r, g, b] = oklchToLinearSRGB(L, 0, hue);
    return [clamp01(r), clamp01(g), clamp01(b)];
  }
  const [r0, g0, b0] = oklchToLinearSRGB(L, wantC, hue);
  if (inSRGBGamut(r0, g0, b0)) {
    return [clamp01(r0), clamp01(g0), clamp01(b0)];
  }
  let lo = 0.0;
  let hi = wantC;
  for (let i = 0; i < 32; i++) {
    const mid = 0.5 * (lo + hi);
    const [mr, mg, mb] = oklchToLinearSRGB(L, mid, hue);
    if (inSRGBGamut(mr, mg, mb)) {
      lo = mid;
    } else {
      hi = mid;
    }
  }
  const [r, g, b] = oklchToLinearSRGB(L, lo, hue);
  return [clamp01(r), clamp01(g), clamp01(b)];
}

function shadeAtTargetLuminance(targetY: number, wantC: number, hue: number): string {
  let loL = 0.0;
  let hiL = 1.0;
  let bestR = 0;
  let bestG = 0;
  let bestB = 0;
  for (let i = 0; i < 40; i++) {
    const midL = 0.5 * (loL + hiL);
    const [lr, lg, lb] = clampGamutOKLCH(midL, wantC, hue);
    bestR = lr;
    bestG = lg;
    bestB = lb;
    const y = linearLuminance(lr, lg, lb);
    if (y < targetY) {
      loL = midL;
    } else {
      hiL = midL;
    }
  }
  const r8 = Math.round(linearChannelToSRGB(bestR) * 255.0);
  const g8 = Math.round(linearChannelToSRGB(bestG) * 255.0);
  const b8 = Math.round(linearChannelToSRGB(bestB) * 255.0);
  return formatHexRGB(r8, g8, b8);
}

/**
 * Generates a deterministic 11-step (`50`..`950`) shade ramp anchored to
 * calibrated WCAG relative luminance targets while preserving the input accent
 * hex's OKLCH hue angle. Matches `GenerateOKLCHScale` in Go.
 */
export function generateOKLCHScale(accentHex: string): BrandingScaleMap | null {
  const rgb = parseHexRGB(accentHex);
  if (!rgb) return null;
  const [, rawC, hue] = srgbToOKLCH(rgb[0], rgb[1], rgb[2]);
  const baseC = rawC < 0.002 ? 0 : rawC;
  const out = {} as BrandingScaleMap;
  for (const anchor of OKLCH_SHADE_ANCHORS) {
    out[anchor.step] = shadeAtTargetLuminance(anchor.targetY, baseC * anchor.chromaFactor, hue);
  }
  return out;
}

function pickAutoForeground(darkFg: string, ...bgs: string[]): string {
  const minContrast = (fg: string): [number, number] => {
    let minR = 21.0;
    let firstR = 21.0;
    for (let i = 0; i < bgs.length; i++) {
      const bg = bgs[i];
      if (!bg) continue;
      const r = contrastRatio(fg, bg);
      if (r === null) continue;
      if (i === 0) firstR = r;
      if (r < minR) minR = r;
    }
    return [minR, firstR];
  };
  const [wMin, wFirst] = minContrast(WHITE_HEX);
  const [dMin, dFirst] = minContrast(darkFg);
  if (dMin > wMin || (dMin === wMin && dFirst > wFirst)) {
    return darkFg;
  }
  return WHITE_HEX;
}

function darkerPrimaryStep(
  scale: BrandingScaleMap,
  baseHex: string,
  fallbackStep: ScaleStep
): string {
  const baseY = relativeLuminance(baseHex);
  if (baseY === null) return scale[fallbackStep];
  for (const st of ['600', '700', '800', '900', '950'] as const) {
    const cand = scale[st];
    if (!cand) continue;
    const y = relativeLuminance(cand);
    if (y !== null && y < baseY - 0.015) return cand;
  }
  return scale[fallbackStep];
}

function lighterPrimaryStep(
  scale: BrandingScaleMap,
  baseHex: string,
  fallbackStep: ScaleStep
): string {
  const baseY = relativeLuminance(baseHex);
  if (baseY === null) return scale[fallbackStep];
  for (const st of ['300', '200', '100', '50'] as const) {
    const cand = scale[st];
    if (!cand) continue;
    const y = relativeLuminance(cand);
    if (y !== null && y > baseY + 0.03) return cand;
  }
  return scale[fallbackStep];
}

function hasAnyNonEmptyString(obj: Record<string, unknown> | null | undefined): boolean {
  if (!obj) return false;
  return Object.values(obj).some((v) => typeof v === 'string' && v !== '');
}

function hasPaletteEntries(palette: RawPaletteConfig | null | undefined): boolean {
  if (!palette) return false;
  return (
    hasAnyNonEmptyString(palette.primary) ||
    hasAnyNonEmptyString(palette.neutral) ||
    hasAnyNonEmptyString(palette.success) ||
    hasAnyNonEmptyString(palette.warning) ||
    hasAnyNonEmptyString(palette.danger) ||
    hasAnyNonEmptyString(palette.light) ||
    hasAnyNonEmptyString(palette.dark)
  );
}

const ALLOWED_STEP_SET = new Set<string>(SCALE_STEPS);
const STEP_ORDER = new Map<string, number>(SCALE_STEPS.map((st, idx) => [st, idx]));

const SEMANTIC_KEYS: readonly (keyof BrandingSemanticTokens)[] = [
  'bg',
  'bg_subtle',
  'surface',
  'surface_raised',
  'text',
  'text_secondary',
  'text_muted',
  'border',
  'primary',
  'primary_hover',
  'primary_active',
  'primary_text',
];

function buildKeyPairs(
  light: BrandingSemanticTokens,
  dark: BrandingSemanticTokens
): ResolvedBrandingPalette['keyPairs'] {
  const makePair = (
    id: string,
    mode: 'light' | 'dark',
    label: string,
    fgHex: string,
    bgHex: string,
    minRatio = WCAG_AA_TEXT_MIN
  ): ContrastPairBadge => {
    const ratio = contrastRatio(fgHex, bgHex) ?? 1.0;
    return {
      id,
      mode,
      label,
      fgHex,
      bgHex,
      ratio,
      minRatio,
      pass: ratio + CONTRAST_EPSILON >= minRatio,
    };
  };

  return {
    light: [
      makePair(
        'light.primary:surface',
        'light',
        'Primary on Surface',
        light.primary,
        light.surface
      ),
      makePair('light.primary:bg', 'light', 'Primary on Background', light.primary, light.bg),
      makePair(
        'light.primary_text:primary',
        'light',
        'Text on Primary',
        light.primary_text,
        light.primary
      ),
    ],
    dark: [
      makePair('dark.primary:surface', 'dark', 'Primary on Surface', dark.primary, dark.surface),
      makePair('dark.primary:bg', 'dark', 'Primary on Background', dark.primary, dark.bg),
      makePair(
        'dark.primary_text:primary',
        'dark',
        'Text on Primary',
        dark.primary_text,
        dark.primary
      ),
    ],
  };
}

/**
 * Resolves effective scales, light/dark semantic tokens, and the full WCAG 2.1 AA
 * contrast check matrix for a branding draft. Mirrors `resolveAndValidatePalette`
 * in `pkg/config/branding_palette.go`.
 */
export function resolveBrandingPalette(input: BrandingPaletteInput): ResolvedBrandingPalette {
  const scales: Record<ScaleName, BrandingScaleMap> = {
    primary: defaultPrimaryScale(),
    neutral: defaultNeutralScale(),
    success: defaultSuccessScale(),
    warning: defaultWarningScale(),
    danger: defaultDangerScale(),
  };
  const light = defaultLightSemanticMode();
  const dark = defaultDarkSemanticMode();
  const syntaxErrors: Array<{ path: string; message: string }> = [];
  const checks: ContrastCheckResult[] = [];

  let accent = (input.accentColor ?? '').trim();
  const palette = input.palette ?? null;
  const hasPalette = hasPaletteEntries(palette);
  const hasCustomPalette = accent !== '' || hasPalette;

  if (!hasCustomPalette) {
    return {
      scales,
      light,
      dark,
      hasCustomPalette: false,
      syntaxErrors,
      checks,
      summary: { total: 0, passed: 0, failed: 0, allPass: true },
      keyPairs: buildKeyPairs(light, dark),
    };
  }

  if (accent !== '') {
    const ramp = generateOKLCHScale(accent);
    if (!ramp) {
      syntaxErrors.push({
        path: `${BRANDING_KOANF_PREFIX}.accent_color`,
        message: 'must be a 6-digit #RRGGBB hex color',
      });
      accent = '';
    } else {
      scales.primary = ramp;
      accent = normalizeHex(accent);
    }
  }

  const validateAndOverlayScale = (
    name: ScaleName,
    override: PartialBrandingScale | null | undefined
  ): boolean => {
    if (!override) return false;
    const keys = Object.keys(override).filter((k) => {
      const v = override[k];
      return typeof v === 'string' && v !== '';
    });
    if (keys.length === 0) return false;
    keys.sort((a, b) => {
      const oi = STEP_ORDER.get(a);
      const oj = STEP_ORDER.get(b);
      if (oi !== undefined && oj !== undefined) return oi - oj;
      if (oi !== undefined) return -1;
      if (oj !== undefined) return 1;
      return a < b ? -1 : a > b ? 1 : 0;
    });
    for (const step of keys) {
      const val = override[step] ?? '';
      const path = `${BRANDING_KOANF_PREFIX}.palette.${name}.${step}`;
      if (!ALLOWED_STEP_SET.has(step)) {
        syntaxErrors.push({
          path,
          message: `unknown shade step "${step}" (allowed: ${SCALE_STEPS.join(', ')})`,
        });
        continue;
      }
      if (!isValidHexColor(val)) {
        syntaxErrors.push({
          path,
          message: 'must be a 6-digit #RRGGBB hex color',
        });
        continue;
      }
      scales[name][step as ScaleStep] = normalizeHex(val);
    }
    return true;
  };

  const customPrimaryScale = validateAndOverlayScale('primary', palette?.primary);
  const customNeutralScale = validateAndOverlayScale('neutral', palette?.neutral);
  const customSuccessScale = validateAndOverlayScale('success', palette?.success);
  const customWarningScale = validateAndOverlayScale('warning', palette?.warning);
  const customDangerScale = validateAndOverlayScale('danger', palette?.danger);

  const validateModeHexes = (
    modeName: 'light' | 'dark',
    m: PartialSemanticTokens | null | undefined
  ): void => {
    if (!m) return;
    for (const key of SEMANTIC_KEYS) {
      const val = m[key];
      if (!val) continue;
      if (!isValidHexColor(val)) {
        syntaxErrors.push({
          path: `${BRANDING_KOANF_PREFIX}.palette.${modeName}.${key}`,
          message: 'must be a 6-digit #RRGGBB hex color',
        });
      }
    }
  };

  const lightIn = palette?.light ?? null;
  const darkIn = palette?.dark ?? null;
  validateModeHexes('light', lightIn);
  validateModeHexes('dark', darkIn);

  if (syntaxErrors.length > 0) {
    return {
      scales,
      light,
      dark,
      hasCustomPalette,
      syntaxErrors,
      checks,
      summary: { total: 0, passed: 0, failed: 0, allPass: true },
      keyPairs: buildKeyPairs(light, dark),
    };
  }

  // Seed primary ramp from explicit mode primary when neither accent_color nor palette.primary is set
  let explicitPrimarySeed = '';
  if (accent === '' && !customPrimaryScale) {
    if (lightIn?.primary) {
      explicitPrimarySeed = normalizeHex(lightIn.primary);
    } else if (darkIn?.primary) {
      explicitPrimarySeed = normalizeHex(darkIn.primary);
    }
    if (explicitPrimarySeed !== '') {
      const ramp = generateOKLCHScale(explicitPrimarySeed);
      if (ramp) scales.primary = ramp;
    }
  }

  const neutral = scales.neutral;
  const primary = scales.primary;

  // Derive semantic backgrounds, surfaces, text, and border from resolved neutral scale
  light.bg = neutral['50'];
  light.bg_subtle = neutral['100'];
  light.surface = WHITE_HEX;
  light.surface_raised = WHITE_HEX;
  light.text = neutral['800'];
  light.text_secondary = neutral['600'];
  light.text_muted = neutral['500'];
  light.border = neutral['200'];

  if (lightIn) {
    if (lightIn.bg) light.bg = normalizeHex(lightIn.bg);
    if (lightIn.bg_subtle) light.bg_subtle = normalizeHex(lightIn.bg_subtle);
    if (lightIn.surface) light.surface = normalizeHex(lightIn.surface);
    if (lightIn.surface_raised) light.surface_raised = normalizeHex(lightIn.surface_raised);
    if (lightIn.text) light.text = normalizeHex(lightIn.text);
    if (lightIn.text_secondary) light.text_secondary = normalizeHex(lightIn.text_secondary);
    if (lightIn.text_muted) light.text_muted = normalizeHex(lightIn.text_muted);
    if (lightIn.border) light.border = normalizeHex(lightIn.border);
  }

  dark.bg = neutral['900'];
  dark.bg_subtle = neutral['800'];
  dark.surface = neutral['800'];
  dark.surface_raised = neutral['700'];
  dark.text = neutral['100'];
  dark.text_secondary = neutral['300'];
  dark.text_muted = neutral['400'];
  dark.border = neutral['700'];

  if (darkIn) {
    if (darkIn.bg) dark.bg = normalizeHex(darkIn.bg);
    if (darkIn.bg_subtle) dark.bg_subtle = normalizeHex(darkIn.bg_subtle);
    if (darkIn.surface) dark.surface = normalizeHex(darkIn.surface);
    if (darkIn.surface_raised) dark.surface_raised = normalizeHex(darkIn.surface_raised);
    if (darkIn.text) dark.text = normalizeHex(darkIn.text);
    if (darkIn.text_secondary) dark.text_secondary = normalizeHex(darkIn.text_secondary);
    if (darkIn.text_muted) dark.text_muted = normalizeHex(darkIn.text_muted);
    if (darkIn.border) dark.border = normalizeHex(darkIn.border);
  }

  const neutralStepCustomized = (step: ScaleStep): boolean =>
    Boolean(palette?.neutral && palette.neutral[step]);

  const modeTokenCustomized = (
    fieldVal: string | undefined,
    backingNeutralStep: ScaleStep | ''
  ): boolean => {
    if (fieldVal) return true;
    return backingNeutralStep !== '' && neutralStepCustomized(backingNeutralStep);
  };

  const basePrimaryChanged = accent !== '' || customPrimaryScale || explicitPrimarySeed !== '';
  const lightPrimaryChanged =
    basePrimaryChanged ||
    Boolean(
      lightIn &&
      (lightIn.primary || lightIn.primary_hover || lightIn.primary_active || lightIn.primary_text)
    );
  const darkPrimaryChanged =
    basePrimaryChanged ||
    Boolean(
      darkIn &&
      (darkIn.primary || darkIn.primary_hover || darkIn.primary_active || darkIn.primary_text)
    );

  const lightBgOrSurfaceCustomized =
    modeTokenCustomized(lightIn?.bg, '50') || modeTokenCustomized(lightIn?.surface, '');
  const darkBgOrSurfaceCustomized =
    modeTokenCustomized(darkIn?.bg, '900') || modeTokenCustomized(darkIn?.surface, '800');

  const recalcLightPrimary = lightPrimaryChanged || lightBgOrSurfaceCustomized;
  const recalcDarkPrimary = darkPrimaryChanged || darkBgOrSurfaceCustomized;

  const modePrimaryRamp = (explicitHex: string): BrandingScaleMap => {
    if (explicitHex && !customPrimaryScale) {
      const ramp = generateOKLCHScale(explicitHex);
      if (ramp) return ramp;
    }
    return primary;
  };

  if (recalcLightPrimary) {
    if (lightIn?.primary) {
      light.primary = normalizeHex(lightIn.primary);
    } else if (accent !== '') {
      const rSurf = contrastRatio(accent, light.surface) ?? 0;
      const rBg = contrastRatio(accent, light.bg) ?? 0;
      if (rSurf >= WCAG_AA_TEXT_MIN && rBg >= WCAG_AA_TEXT_MIN) {
        light.primary = accent;
      } else {
        light.primary = primary['600'];
      }
    } else {
      light.primary = primary['600'];
    }

    const lightRamp = lightIn?.primary ? modePrimaryRamp(light.primary) : primary;

    if (lightIn?.primary_hover) {
      light.primary_hover = normalizeHex(lightIn.primary_hover);
    } else {
      light.primary_hover = darkerPrimaryStep(lightRamp, light.primary, '700');
    }

    if (lightIn?.primary_active) {
      light.primary_active = normalizeHex(lightIn.primary_active);
    } else {
      const activeRamp =
        !lightIn?.primary && lightIn?.primary_hover
          ? modePrimaryRamp(light.primary_hover)
          : lightRamp;
      light.primary_active = darkerPrimaryStep(activeRamp, light.primary_hover, '800');
    }

    if (lightIn?.primary_text) {
      light.primary_text = normalizeHex(lightIn.primary_text);
    } else {
      light.primary_text = pickAutoForeground(
        neutral['950'],
        light.primary,
        light.primary_hover,
        light.primary_active
      );
    }
  }

  if (recalcDarkPrimary) {
    if (darkIn?.primary) {
      dark.primary = normalizeHex(darkIn.primary);
    } else if (accent !== '') {
      const rSurf = contrastRatio(accent, dark.surface) ?? 0;
      const rBg = contrastRatio(accent, dark.bg) ?? 0;
      if (rSurf >= WCAG_AA_TEXT_MIN && rBg >= WCAG_AA_TEXT_MIN) {
        dark.primary = accent;
      } else {
        dark.primary = primary['400'];
      }
    } else {
      dark.primary = primary['400'];
    }

    const darkRamp = darkIn?.primary ? modePrimaryRamp(dark.primary) : primary;

    if (darkIn?.primary_hover) {
      dark.primary_hover = normalizeHex(darkIn.primary_hover);
    } else {
      dark.primary_hover = lighterPrimaryStep(darkRamp, dark.primary, '300');
    }

    if (darkIn?.primary_active) {
      dark.primary_active = normalizeHex(darkIn.primary_active);
    } else {
      const activeRamp =
        !darkIn?.primary && darkIn?.primary_hover ? modePrimaryRamp(dark.primary_hover) : darkRamp;
      dark.primary_active = lighterPrimaryStep(activeRamp, dark.primary_hover, '200');
    }

    if (darkIn?.primary_text) {
      dark.primary_text = normalizeHex(darkIn.primary_text);
    } else {
      dark.primary_text = pickAutoForeground(
        neutral['950'],
        dark.primary,
        dark.primary_hover,
        dark.primary_active
      );
    }
  }

  // --- WCAG 2.1 AA Contrast Matrix (exact server parity) ---

  const addCheck = (
    id: string,
    path: string,
    mode: 'light' | 'dark',
    label: string,
    fgHex: string,
    targetLabel: string,
    bgHex: string,
    minRatio: number
  ): void => {
    const ratio = contrastRatio(fgHex, bgHex);
    if (ratio === null) return;
    const pass = ratio + CONTRAST_EPSILON >= minRatio;
    checks.push({
      id,
      path,
      mode,
      label,
      targetLabel,
      fgHex,
      bgHex,
      ratio,
      minRatio,
      pass,
      message: pass
        ? `${label}: ${formatContrastRatio(ratio)} (WCAG AA >= ${formatContrastRatio(minRatio)})`
        : `contrast (${fgHex}) against ${targetLabel} (${bgHex}) is ${formatContrastRatio(ratio)}, minimum WCAG AA is ${formatContrastRatio(minRatio)}`,
    });
  };

  // 1. Body, secondary, and muted text vs backgrounds & surfaces (4.5:1)
  const lightTexts = [
    { name: 'text', hex: light.text, customized: modeTokenCustomized(lightIn?.text, '800') },
    {
      name: 'text_secondary',
      hex: light.text_secondary,
      customized: modeTokenCustomized(lightIn?.text_secondary, '600'),
    },
    {
      name: 'text_muted',
      hex: light.text_muted,
      customized: modeTokenCustomized(lightIn?.text_muted, '500'),
    },
  ];
  const lightBgs = [
    { name: 'bg', hex: light.bg, customized: modeTokenCustomized(lightIn?.bg, '50') },
    {
      name: 'bg_subtle',
      hex: light.bg_subtle,
      customized: modeTokenCustomized(lightIn?.bg_subtle, '100'),
    },
    { name: 'surface', hex: light.surface, customized: modeTokenCustomized(lightIn?.surface, '') },
    {
      name: 'surface_raised',
      hex: light.surface_raised,
      customized: modeTokenCustomized(lightIn?.surface_raised, ''),
    },
  ];
  for (const tTok of lightTexts) {
    for (const bTok of lightBgs) {
      if (tTok.customized || bTok.customized) {
        addCheck(
          `light.${tTok.name}:light.${bTok.name}`,
          `${BRANDING_KOANF_PREFIX}.palette.light.${tTok.name}`,
          'light',
          `Light ${tTok.name} on ${bTok.name}`,
          tTok.hex,
          `light.${bTok.name}`,
          bTok.hex,
          WCAG_AA_TEXT_MIN
        );
      }
    }
  }

  const darkTexts = [
    { name: 'text', hex: dark.text, customized: modeTokenCustomized(darkIn?.text, '100') },
    {
      name: 'text_secondary',
      hex: dark.text_secondary,
      customized: modeTokenCustomized(darkIn?.text_secondary, '300'),
    },
    {
      name: 'text_muted',
      hex: dark.text_muted,
      customized: modeTokenCustomized(darkIn?.text_muted, '400'),
    },
  ];
  const darkBgs = [
    { name: 'bg', hex: dark.bg, customized: modeTokenCustomized(darkIn?.bg, '900') },
    {
      name: 'bg_subtle',
      hex: dark.bg_subtle,
      customized: modeTokenCustomized(darkIn?.bg_subtle, '800'),
    },
    { name: 'surface', hex: dark.surface, customized: modeTokenCustomized(darkIn?.surface, '800') },
    {
      name: 'surface_raised',
      hex: dark.surface_raised,
      customized: modeTokenCustomized(darkIn?.surface_raised, '700'),
    },
  ];
  for (const tTok of darkTexts) {
    for (const bTok of darkBgs) {
      if (tTok.customized || bTok.customized) {
        addCheck(
          `dark.${tTok.name}:dark.${bTok.name}`,
          `${BRANDING_KOANF_PREFIX}.palette.dark.${tTok.name}`,
          'dark',
          `Dark ${tTok.name} on ${bTok.name}`,
          tTok.hex,
          `dark.${bTok.name}`,
          bTok.hex,
          WCAG_AA_TEXT_MIN
        );
      }
    }
  }

  // 2 & 3. Primary vs bg/surface and primary_text vs primary/hover/active (4.5:1)
  if (recalcLightPrimary) {
    addCheck(
      'light.primary:light.bg',
      `${BRANDING_KOANF_PREFIX}.palette.light.primary`,
      'light',
      'Light primary on bg',
      light.primary,
      'light.bg',
      light.bg,
      WCAG_AA_TEXT_MIN
    );
    addCheck(
      'light.primary:light.surface',
      `${BRANDING_KOANF_PREFIX}.palette.light.primary`,
      'light',
      'Light primary on surface',
      light.primary,
      'light.surface',
      light.surface,
      WCAG_AA_TEXT_MIN
    );
    addCheck(
      'light.primary_text:light.primary',
      `${BRANDING_KOANF_PREFIX}.palette.light.primary_text`,
      'light',
      'Light primary text on primary',
      light.primary_text,
      'light.primary',
      light.primary,
      WCAG_AA_TEXT_MIN
    );
    addCheck(
      'light.primary_text:light.primary_hover',
      `${BRANDING_KOANF_PREFIX}.palette.light.primary_text`,
      'light',
      'Light primary text on hover',
      light.primary_text,
      'light.primary_hover',
      light.primary_hover,
      WCAG_AA_TEXT_MIN
    );
    addCheck(
      'light.primary_text:light.primary_active',
      `${BRANDING_KOANF_PREFIX}.palette.light.primary_text`,
      'light',
      'Light primary text on active',
      light.primary_text,
      'light.primary_active',
      light.primary_active,
      WCAG_AA_TEXT_MIN
    );
  }
  if (recalcDarkPrimary) {
    addCheck(
      'dark.primary:dark.bg',
      `${BRANDING_KOANF_PREFIX}.palette.dark.primary`,
      'dark',
      'Dark primary on bg',
      dark.primary,
      'dark.bg',
      dark.bg,
      WCAG_AA_TEXT_MIN
    );
    addCheck(
      'dark.primary:dark.surface',
      `${BRANDING_KOANF_PREFIX}.palette.dark.primary`,
      'dark',
      'Dark primary on surface',
      dark.primary,
      'dark.surface',
      dark.surface,
      WCAG_AA_TEXT_MIN
    );
    addCheck(
      'dark.primary_text:dark.primary',
      `${BRANDING_KOANF_PREFIX}.palette.dark.primary_text`,
      'dark',
      'Dark primary text on primary',
      dark.primary_text,
      'dark.primary',
      dark.primary,
      WCAG_AA_TEXT_MIN
    );
    addCheck(
      'dark.primary_text:dark.primary_hover',
      `${BRANDING_KOANF_PREFIX}.palette.dark.primary_text`,
      'dark',
      'Dark primary text on hover',
      dark.primary_text,
      'dark.primary_hover',
      dark.primary_hover,
      WCAG_AA_TEXT_MIN
    );
    addCheck(
      'dark.primary_text:dark.primary_active',
      `${BRANDING_KOANF_PREFIX}.palette.dark.primary_text`,
      'dark',
      'Dark primary text on active',
      dark.primary_text,
      'dark.primary_active',
      dark.primary_active,
      WCAG_AA_TEXT_MIN
    );
  }

  // 4 & 5. Shoelace filled button/badge contrast & status badge contrast
  const ramps: Array<{ name: ScaleName; customized: boolean; lightBgKey: ScaleStep }> = [
    { name: 'primary', customized: basePrimaryChanged, lightBgKey: '100' },
    { name: 'neutral', customized: customNeutralScale, lightBgKey: '200' },
    { name: 'success', customized: customSuccessScale, lightBgKey: '100' },
    { name: 'warning', customized: customWarningScale, lightBgKey: '100' },
    { name: 'danger', customized: customDangerScale, lightBgKey: '100' },
  ];

  const rampStepCustomized = (rampName: ScaleName, step: ScaleStep): boolean => {
    if (rampName === 'primary' && (accent !== '' || explicitPrimarySeed !== '')) {
      return true;
    }
    const m = palette?.[rampName];
    return Boolean(m && m[step]);
  };

  const darkSurfaceCustomized = modeTokenCustomized(darkIn?.surface, '800');

  for (const rc of ramps) {
    const sc = scales[rc.name];
    if (rampStepCustomized(rc.name, '600')) {
      addCheck(
        `light.sl-${rc.name}-600`,
        `${BRANDING_KOANF_PREFIX}.palette.${rc.name}.600`,
        'light',
        `Light ${rc.name} filled button (600)`,
        WHITE_HEX,
        `light --sl-color-${rc.name}-600`,
        sc['600'],
        WCAG_AA_TEXT_MIN
      );
    }
    if (rampStepCustomized(rc.name, '300') || neutralStepCustomized('800')) {
      addCheck(
        `dark.sl-${rc.name}-300`,
        `${BRANDING_KOANF_PREFIX}.palette.${rc.name}.300`,
        'dark',
        `Dark ${rc.name} filled button (300)`,
        neutral['800'],
        `dark --sl-color-${rc.name}-600 (step 300)`,
        sc['300'],
        WCAG_AA_TEXT_MIN
      );
    }
    if (rampStepCustomized(rc.name, '400') || neutralStepCustomized('800')) {
      addCheck(
        `dark.sl-${rc.name}-400`,
        `${BRANDING_KOANF_PREFIX}.palette.${rc.name}.400`,
        'dark',
        `Dark ${rc.name} hover button (400)`,
        neutral['800'],
        `dark --sl-color-${rc.name}-500 hover (step 400)`,
        sc['400'],
        WCAG_AA_TEXT_MIN
      );
    }
    if (rc.customized) {
      addCheck(
        `light.badge-${rc.name}`,
        `${BRANDING_KOANF_PREFIX}.palette.${rc.name}.800`,
        'light',
        `Light ${rc.name} badge (800 on ${rc.lightBgKey})`,
        sc['800'],
        `light badge ${rc.name}-${rc.lightBgKey}`,
        sc[rc.lightBgKey],
        WCAG_AA_TEXT_MIN
      );
    }
    if (rc.customized || darkSurfaceCustomized) {
      const ratio = compositeAlphaContrast(sc['300'], sc['500'], 0.2, dark.surface);
      if (ratio !== null) {
        const pass = ratio + CONTRAST_EPSILON >= WCAG_AA_TEXT_MIN;
        const compositeBg = compositeAlphaHex(sc['500'], 0.2, dark.surface);
        checks.push({
          id: `dark.badge-${rc.name}`,
          path: `${BRANDING_KOANF_PREFIX}.palette.${rc.name}.300`,
          mode: 'dark',
          label: `Dark ${rc.name} badge (300 on 20% 500)`,
          targetLabel: `20% ${sc['500']} over dark.surface ${dark.surface}`,
          fgHex: sc['300'],
          bgHex: compositeBg,
          ratio,
          minRatio: WCAG_AA_TEXT_MIN,
          pass,
          message: pass
            ? `Dark ${rc.name} badge: ${formatContrastRatio(ratio)} (WCAG AA >= 4.50:1)`
            : `dark badge contrast (${sc['300']} on 20% ${sc['500']} over dark.surface ${dark.surface}) is ${formatContrastRatio(ratio)}, minimum WCAG AA is ${formatContrastRatio(WCAG_AA_TEXT_MIN)}`,
        });
      }
    }
  }

  // 6. Non-text UI component boundary contrast (3.0:1): neutral borders vs surface
  if (neutralStepCustomized('600') || modeTokenCustomized(lightIn?.surface, '')) {
    addCheck(
      'light.ui-border',
      `${BRANDING_KOANF_PREFIX}.palette.neutral.600`,
      'light',
      'Light UI border (neutral-600 on surface)',
      neutral['600'],
      'light.surface (UI border contrast)',
      light.surface,
      WCAG_AA_UI_MIN
    );
  }
  if (neutralStepCustomized('400') || darkSurfaceCustomized) {
    addCheck(
      'dark.ui-border',
      `${BRANDING_KOANF_PREFIX}.palette.neutral.400`,
      'dark',
      'Dark UI border (neutral-400 on surface)',
      neutral['400'],
      'dark.surface (UI border contrast)',
      dark.surface,
      WCAG_AA_UI_MIN
    );
  }

  const failed = checks.filter((c) => !c.pass).length;
  return {
    scales,
    light,
    dark,
    hasCustomPalette: true,
    syntaxErrors,
    checks,
    summary: {
      total: checks.length,
      passed: checks.length - failed,
      failed,
      allPass: failed === 0,
    },
    keyPairs: buildKeyPairs(light, dark),
  };
}
