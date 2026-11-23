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
 * Hub Admin Branding Page (/admin/branding)
 *
 * Configures Hub-wide UI branding settings (`server.hub.branding.*`):
 * Identity & Attribution, Shape & Typography, Logos & Icons (URL or
 * client-side file-to-data: upload with 128 KiB / 512 KiB budget meter),
 * and Accent Color. Saves via dirty-diff PUT to `/api/v1/admin/server-config`
 * with CAS `expected_revisions`, preserves untouched CLI `palette.*` keys,
 * and triggers live SPA reload on save/reset.
 */

import { LitElement, html, css, nothing, type TemplateResult } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';
import { loadAdminStatus } from '../../client/admin-status.js';
import { getPublicSettings, invalidatePublicSettings } from '../../client/server-feature-flags.js';
import { stateManager } from '../../client/state.js';
import { hasAnyPermission } from '../../lib/admin-permissions.js';
import {
  classifySettingsError,
  fieldErrorMessages,
  genericSaveErrorText,
  sectionPathsFromSchema,
  type SettingsSaveError,
} from '../../shared/settings-save-errors.js';
import { revisionsFromMetadata } from '../../shared/settings-dirty.js';
import {
  renderConflictNotice,
  renderFieldErrorMessages,
  renderLayer0Notice,
  renderSectionErrorSummary,
  settingsErrorStyles,
} from '../shared/settings-error-views.js';
import {
  envBadgeStyles,
  readOnlyFieldStyles,
  renderEnvBadge,
  renderEnvLockBadge,
  renderKeysEnvBadge,
} from '../shared/env-badge-views.js';
import {
  type EnvKeyValue,
  envValueFor,
  formatEnvValue,
  hasInEffect,
} from './admin-server-config-env-values.js';
import {
  ASSET_SLOTS,
  BRANDING_KOANF_PREFIX,
  CUSTOM_FONT_SENTINEL,
  DEFAULT_ACCENT_PICKER_COLOR,
  FONT_SANS_OPTIONS,
  HEX_COLOR_RE,
  MAX_BRAND_NAME_RUNES,
  MAX_BRAND_SHORT_NAME_RUNES,
  MAX_BRAND_SUBTITLE_RUNES,
  MAX_BRAND_TOTAL_BYTES,
  brandingFromConfig,
  buildBrandingDiff,
  buildFileModeResetPayload,
  cloneBrandingFormState,
  defaultBrandingFormState,
  formatKiB,
  getSlotValue,
  hasAdvancedPaletteOverrides,
  hasAnyConfiguredBranding,
  isBrandingDirty,
  isCuratedOrBuiltinFont,
  normalizeAssetDataUrl,
  parseDataUriInfo,
  setSlotValue,
  totalDataUriBytes,
  validateAccentColorInput,
  validateAssetFile,
  validateAssetUrlInput,
  validateFontSansInput,
  type AssetSlotDef,
  type BrandCorners,
  type BrandingFormState,
  type RawBrandingConfig,
} from './admin-branding-state.js';
import {
  SCALE_STEPS,
  formatContrastRatio,
  resolveBrandingPalette,
  type ContrastPairBadge,
  type RawPaletteConfig,
  type ResolvedBrandingPalette,
} from './admin-branding-contrast.js';
import './admin-branding-preview.js';

/** Primary value of an env-locked field whose environment value GET does not report. */
export const ENV_VALUE_NOT_REPORTED = 'Environment value not reported';

interface SectionMetadataInfo {
  source?: string;
  revision?: number;
  updated_at?: string;
  updated_by?: string;
  origin?: string;
}

interface ServerConfigBrandingResponse {
  server?: {
    hub?: {
      branding?: RawBrandingConfig | null;
    };
  };
  settings_tier?: 'db' | 'file';
  env_overrides?: string[];
  env_values?: Record<string, EnvKeyValue>;
  section_metadata?: Record<string, SectionMetadataInfo>;
}

/** Reads a File as a `data:` URL in the browser. */
function readFileAsDataUrl(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => {
      if (typeof reader.result === 'string') {
        resolve(reader.result);
      } else {
        reject(new Error('Failed to read file as data URL'));
      }
    };
    reader.onerror = () => reject(reader.error ?? new Error('Failed to read file'));
    reader.readAsDataURL(file);
  });
}

@customElement('scion-page-admin-branding')
export class ScionPageAdminBranding extends LitElement {
  @state() private loading = true;
  @state() private saving = false;
  @state() private resetting = false;
  @state() private error: string | null = null;
  @state() private successMessage: string | null = null;
  @state() private saveError: SettingsSaveError | null = null;

  @state() private canEdit = false;
  @state() private permissionsLoaded = false;

  @state() private draft: BrandingFormState = defaultBrandingFormState();
  @state() private baseline: BrandingFormState = defaultBrandingFormState();
  @state() private rawPalette: RawPaletteConfig | null = null;
  @state() private hasPaletteOverrides = false;
  @state() private customFontMode = false;

  @state() private settingsTier: 'db' | 'file' | undefined = undefined;
  @state() private envOverrides: string[] = [];
  @state() private envValues: Record<string, EnvKeyValue> = {};
  @state() private sectionMetadata: SectionMetadataInfo | null = null;

  @state() private resetDialogOpen = false;
  @state() private resetError: string | null = null;
  @state() private slotUploadErrors: Record<string, string> = {};

  private brandingRevision: number | null = null;
  private sectionKoanfPathsPromise: Promise<Record<string, string[]> | undefined> | null = null;
  private successMessageTimer: ReturnType<typeof setTimeout> | null = null;

  static override styles = css`
    ${settingsErrorStyles}
    ${envBadgeStyles}
    ${readOnlyFieldStyles}

    :host {
      display: block;
      padding-bottom: 5rem;
    }

    .header {
      display: flex;
      align-items: flex-start;
      justify-content: space-between;
      gap: 1rem;
      margin-bottom: 0.5rem;
      flex-wrap: wrap;
    }

    .header-left {
      display: flex;
      align-items: center;
      gap: 0.75rem;
    }

    .header sl-icon {
      color: var(--scion-primary, #3b82f6);
      font-size: 1.5rem;
    }

    .header h1 {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }

    .header-description {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
      margin: 0 0 0.75rem 0;
    }

    .provenance-caption {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 1.25rem 0;
      display: flex;
      align-items: center;
      gap: 0.5rem;
      flex-wrap: wrap;
    }

    .section {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.5rem;
      margin-bottom: 1.5rem;
    }

    .section-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 1rem;
      margin: 0 0 1rem 0;
      padding-bottom: 0.75rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      flex-wrap: wrap;
    }

    .section-title {
      font-size: 1rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }

    .form-grid {
      display: grid;
      grid-template-columns: 1fr 1fr;
      gap: 1.25rem;
    }

    @media (max-width: 768px) {
      .form-grid {
        grid-template-columns: 1fr;
      }
    }

    .form-field {
      display: flex;
      flex-direction: column;
      gap: 0.375rem;
    }

    .form-field.full-width {
      grid-column: 1 / -1;
    }

    .field-label-row {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 0.5rem;
      min-height: 1.5rem;
    }

    .field-label-row label,
    .field-label-text {
      font-size: 0.8125rem;
      font-weight: 500;
      color: var(--scion-text, #1e293b);
    }

    .hint {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }

    .loading-container {
      display: flex;
      justify-content: center;
      align-items: center;
      padding: 4rem;
    }

    .status-message {
      font-size: 0.875rem;
      padding: 0.75rem 1rem;
      border-radius: var(--scion-radius, 0.5rem);
      margin-bottom: 1rem;
    }

    .status-message.success {
      background: var(--scion-success-bg, #dcfce7);
      color: var(--scion-success-text, #166534);
      border: 1px solid var(--scion-success-border, #86efac);
    }

    .status-message.error {
      background: var(--scion-error-bg, #fef2f2);
      color: var(--scion-error-text, #991b1b);
      border: 1px solid var(--scion-error-border, #fca5a5);
    }

    .readonly-banner {
      margin-bottom: 1rem;
    }

    .palette-notice {
      margin-top: 1rem;
    }

    sl-input::part(base),
    sl-select::part(combobox) {
      font-size: 0.875rem;
      border-color: var(--scion-border, #e2e8f0);
      background: var(--scion-surface, #ffffff);
    }

    @media (pointer: coarse) {
      sl-input::part(base),
      sl-select::part(combobox) {
        font-size: max(16px, 0.875rem);
      }
    }

    sl-input::part(input) {
      color: var(--scion-text, #1e293b);
    }

    /* Hide Shoelace control internal label when we render our own label row with reset action */
    .sr-control-label::part(form-control-label) {
      position: absolute;
      width: 1px;
      height: 1px;
      padding: 0;
      margin: -1px;
      overflow: hidden;
      clip: rect(0, 0, 0, 0);
      white-space: nowrap;
      border: 0;
    }

    .sr-only {
      position: absolute;
      width: 1px;
      height: 1px;
      padding: 0;
      margin: -1px;
      overflow: hidden;
      clip: rect(0, 0, 0, 0);
      white-space: nowrap;
      border: 0;
    }

    .dimension-hint {
      display: inline-block;
      margin-left: 0.375rem;
      padding: 0.0625rem 0.5rem;
      font-size: 0.6875rem;
      font-weight: 500;
      color: var(--scion-text-muted, #64748b);
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 9999px;
    }

    .corner-option {
      display: inline-flex;
      align-items: center;
      gap: 0.375rem;
    }

    .corner-glyph {
      width: 1rem;
      height: 1rem;
      flex-shrink: 0;
    }

    .custom-font-input {
      margin-top: 0.5rem;
    }

    /* Asset budget meter */
    .budget-meter {
      display: flex;
      flex-direction: column;
      gap: 0.375rem;
      padding: 0.75rem 1rem;
      margin-bottom: 1.25rem;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      background: var(--scion-bg, #f8fafc);
    }

    .budget-meter-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 0.5rem;
      font-size: 0.8125rem;
      color: var(--scion-text, #1e293b);
      flex-wrap: wrap;
    }

    .budget-meter sl-progress-bar {
      --height: 0.5rem;
    }

    .budget-meter.over-limit sl-progress-bar {
      --indicator-color: var(--scion-danger-500, #ef4444);
    }

    .budget-meter.near-limit sl-progress-bar {
      --indicator-color: var(--scion-warning-500, #f59e0b);
    }

    .asset-subsection-title {
      font-size: 0.8125rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.04em;
      color: var(--scion-text-muted, #64748b);
      margin: 1rem 0 0.75rem 0;
    }

    .asset-subsection-title:first-of-type {
      margin-top: 0;
    }

    .asset-list {
      display: flex;
      flex-direction: column;
      gap: 1rem;
    }

    .asset-row {
      display: grid;
      grid-template-columns: auto 1fr;
      gap: 1rem;
      align-items: start;
      padding: 0.875rem;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      background: var(--scion-bg, #f8fafc);
    }

    @media (max-width: 640px) {
      .asset-row {
        grid-template-columns: 1fr;
      }
    }

    /* Split light/dark logo swatch using real theme classes (.sl-theme-light / .sl-theme-dark) */
    .logo-swatch {
      display: grid;
      grid-template-columns: 1fr 1fr;
      width: 112px;
      height: 48px;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-sm, 0.25rem);
      overflow: hidden;
      flex-shrink: 0;
    }

    .logo-swatch-half {
      display: flex;
      align-items: center;
      justify-content: center;
      padding: 0.25rem;
      background: var(--scion-surface, #ffffff);
      color: var(--scion-text-muted, #64748b);
    }

    .logo-swatch-half img {
      max-width: 100%;
      max-height: 36px;
      object-fit: contain;
    }

    .icon-swatch {
      width: 48px;
      height: 48px;
      display: flex;
      align-items: center;
      justify-content: center;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-sm, 0.25rem);
      background: var(--scion-surface, #ffffff);
      color: var(--scion-text-muted, #64748b);
      flex-shrink: 0;
    }

    .icon-swatch img {
      width: 32px;
      height: 32px;
      object-fit: contain;
    }

    .asset-controls {
      display: flex;
      flex-direction: column;
      gap: 0.375rem;
      min-width: 0;
    }

    .asset-input-row {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      flex-wrap: wrap;
    }

    .asset-input-row sl-input {
      flex: 1;
      min-width: 12rem;
    }

    .data-uri-chip {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 0.75rem;
      padding: 0.5rem 0.75rem;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      background: var(--scion-surface, #ffffff);
      flex-wrap: wrap;
    }

    .data-uri-meta {
      display: inline-flex;
      align-items: center;
      gap: 0.5rem;
      font-size: 0.8125rem;
      color: var(--scion-text, #1e293b);
    }

    .data-uri-actions {
      display: inline-flex;
      align-items: center;
      gap: 0.375rem;
      flex-wrap: wrap;
    }

    /* Accent color row */
    .color-input-row {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      max-width: 24rem;
    }

    .color-picker-native {
      width: 2.5rem;
      height: 2.5rem;
      padding: 0.125rem;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      background: var(--scion-surface, #ffffff);
      cursor: pointer;
      flex-shrink: 0;
    }

    .color-picker-native:disabled {
      opacity: 0.5;
      cursor: not-allowed;
    }

    .color-input-row sl-input {
      flex: 1;
    }

    /* Generated 11-step OKLCH primary swatch strip */
    .ramp-container {
      margin-top: 1rem;
      display: flex;
      flex-direction: column;
      gap: 0.375rem;
    }

    .ramp-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 0.5rem;
      font-size: 0.75rem;
      font-weight: 500;
      color: var(--scion-text-secondary, #475569);
    }

    .ramp-strip {
      display: grid;
      grid-template-columns: repeat(11, minmax(0, 1fr));
      gap: 0.25rem;
    }

    @media (max-width: 640px) {
      .ramp-strip {
        grid-template-columns: repeat(6, minmax(0, 1fr));
      }
    }

    .ramp-swatch {
      display: flex;
      flex-direction: column;
      align-items: center;
      gap: 0.25rem;
      min-width: 0;
    }

    .ramp-swatch-chip {
      width: 100%;
      height: 1.75rem;
      border-radius: var(--scion-radius-sm, 0.25rem);
      border: 1px solid var(--scion-border, #e2e8f0);
      background: var(--swatch-color, var(--scion-primary, #3b82f6));
    }

    .ramp-swatch-step {
      font-size: 0.6875rem;
      font-family: var(--scion-font-mono, monospace);
      color: var(--scion-text-muted, #64748b);
    }

    /* WCAG 2.1 AA live contrast badges */
    .contrast-section {
      margin-top: 1rem;
      display: grid;
      grid-template-columns: 1fr 1fr;
      gap: 0.875rem;
    }

    @media (max-width: 768px) {
      .contrast-section {
        grid-template-columns: 1fr;
      }
    }

    .contrast-group {
      padding: 0.75rem;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      background: var(--scion-bg, #f8fafc);
      display: flex;
      flex-direction: column;
      gap: 0.5rem;
    }

    .contrast-group-title {
      display: inline-flex;
      align-items: center;
      gap: 0.375rem;
      font-size: 0.75rem;
      font-weight: 600;
      color: var(--scion-text-secondary, #475569);
    }

    .contrast-badge-list {
      display: flex;
      flex-direction: column;
      gap: 0.375rem;
    }

    .contrast-row {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 0.5rem;
      font-size: 0.75rem;
      color: var(--scion-text, #1e293b);
      flex-wrap: wrap;
    }

    .contrast-pair-meta {
      display: inline-flex;
      align-items: center;
      gap: 0.375rem;
      min-width: 0;
    }

    .contrast-swatch-dot {
      display: inline-flex;
      align-items: center;
      justify-content: center;
      width: 1.25rem;
      height: 1.25rem;
      border-radius: var(--scion-radius-sm, 0.25rem);
      border: 1px solid var(--scion-border, #e2e8f0);
      background: var(--pair-bg, var(--scion-surface, #ffffff));
      color: var(--pair-fg, var(--scion-text, #1e293b));
      font-size: 0.625rem;
      font-weight: 700;
      flex-shrink: 0;
    }

    .contrast-badge-text {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
    }

    .contrast-advisory {
      margin-top: 1rem;
      padding: 0.75rem 1rem;
      border-radius: var(--scion-radius, 0.5rem);
      border: 1px solid var(--scion-warning-border, #fcd34d);
      background: var(--scion-warning-bg, #fffbeb);
      color: var(--scion-warning-text, #92400e);
      font-size: 0.8125rem;
    }

    .contrast-advisory-header {
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }

    .contrast-advisory ul {
      margin: 0.375rem 0 0 1.25rem;
      padding: 0;
      font-size: 0.8125rem;
    }

    /* Sticky save bar */
    .sticky-save-bar {
      position: sticky;
      bottom: 0;
      z-index: 20;
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 1rem;
      padding: 0.875rem 1.25rem;
      background: var(--scion-surface-raised, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      box-shadow: var(--scion-shadow-md, 0 4px 12px rgba(0, 0, 0, 0.08));
      flex-wrap: wrap;
    }

    .sticky-save-status {
      font-size: 0.875rem;
      color: var(--scion-text-secondary, #475569);
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }

    .sticky-save-actions {
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }

    .dialog-footer {
      display: flex;
      justify-content: flex-end;
      gap: 0.5rem;
      margin-top: 1rem;
    }

    .reset-btn::part(base) {
      font-size: 0.875rem;
      padding: 0.125rem;
      color: var(--scion-text-muted, #64748b);
    }

    .reset-btn::part(base):hover {
      color: var(--scion-text, #1e293b);
    }
  `;

  // --- Lifecycle ---

  override connectedCallback(): void {
    super.connectedCallback();
    void this.initialize();
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    if (this.successMessageTimer !== null) {
      clearTimeout(this.successMessageTimer);
      this.successMessageTimer = null;
    }
  }

  private async initialize(): Promise<void> {
    await Promise.all([this.loadPermissions(), this.loadData(true)]);
  }

  private async loadPermissions(): Promise<void> {
    const status = await loadAdminStatus(stateManager.getCurrentUserId());
    this.canEdit = hasAnyPermission(status, ['hub.config.update']);
    this.permissionsLoaded = true;
  }

  // --- Schema & Error Classification ---

  private sectionKoanfPaths(): Promise<Record<string, string[]> | undefined> {
    this.sectionKoanfPathsPromise ??= (async () => {
      try {
        const res = await apiFetch('/api/v1/admin/server-config/schema');
        return res.ok ? sectionPathsFromSchema(await res.json()) : undefined;
      } catch {
        return undefined;
      }
    })();
    return this.sectionKoanfPathsPromise;
  }

  private async classifySaveError(res: Response): Promise<SettingsSaveError> {
    let body: unknown;
    try {
      body = await res.json();
    } catch {
      body = undefined;
    }
    const ctx = { defaultSection: 'branding' };
    const err = classifySettingsError(res.status, body, ctx);
    if (err.kind !== 'validation') return err;
    const sectionKoanfPaths = await this.sectionKoanfPaths();
    return classifySettingsError(res.status, body, { ...ctx, sectionKoanfPaths });
  }

  // --- Data Loading ---

  private async loadData(showSpinner = true): Promise<void> {
    if (showSpinner) {
      this.loading = true;
    }
    this.error = null;
    try {
      const res = await apiFetch('/api/v1/admin/server-config');
      if (!res.ok) {
        this.error = await extractApiError(res, 'Failed to load branding configuration');
        return;
      }
      const data = (await res.json()) as ServerConfigBrandingResponse;
      const rawBranding = data.server?.hub?.branding ?? null;
      const loaded = brandingFromConfig(rawBranding);
      this.baseline = cloneBrandingFormState(loaded);
      this.draft = cloneBrandingFormState(loaded);
      this.rawPalette = (rawBranding?.palette as RawPaletteConfig | undefined) ?? null;
      this.hasPaletteOverrides = hasAdvancedPaletteOverrides(rawBranding);
      this.customFontMode = !isCuratedOrBuiltinFont(loaded.font_sans);

      this.settingsTier = data.settings_tier;
      this.envOverrides = data.env_overrides ?? [];
      this.envValues = data.env_values ?? {};
      this.sectionMetadata = data.section_metadata?.branding ?? null;
      this.brandingRevision =
        data.settings_tier === 'db'
          ? (revisionsFromMetadata(data.section_metadata).branding ?? 0)
          : null;
      this.slotUploadErrors = {};
    } catch {
      this.error = 'Failed to connect to server';
    } finally {
      this.loading = false;
    }
  }

  // --- Env Override Helpers ---

  private envLocked(relPath: string): boolean {
    const key = `${BRANDING_KOANF_PREFIX}.${relPath}`;
    if (!this.envOverrides.includes(key)) return false;
    const found = envValueFor(this.envValues, [key]);
    if (found && hasInEffect(found.ev)) return found.ev.in_effect;
    return this.settingsTier !== 'db';
  }

  private envLockedSet(): Set<string> {
    const locked = new Set<string>();
    const allRelPaths = [
      'name',
      'short_name',
      'subtitle',
      'attribution',
      'corners',
      'font_sans',
      'accent_color',
      ...ASSET_SLOTS.map((s) => s.key),
    ];
    for (const rel of allRelPaths) {
      if (this.envLocked(rel)) {
        locked.add(`${BRANDING_KOANF_PREFIX}.${rel}`);
      }
    }
    return locked;
  }

  private renderFieldWithEnv(
    relPath: string,
    storedDisplay: string,
    control: TemplateResult
  ): TemplateResult {
    const key = `${BRANDING_KOANF_PREFIX}.${relPath}`;
    if (this.envLocked(relPath)) {
      const found = envValueFor(this.envValues, [key]);
      const ev = found?.ev;
      return html`
        <div>${renderEnvLockBadge()} ${renderEnvBadge()}</div>
        <span class="read-only-value env-value-primary"
          >${ev ? formatEnvValue(ev) || '—' : ENV_VALUE_NOT_REPORTED}</span
        >
        ${ev?.saved_differs
          ? html`<span class="hint env-value-saved"
              >${this.settingsTier === 'db' ? 'Saved value' : 'settings.yaml value'}, not in effect
              on this node: <code>${storedDisplay || '—'}</code></span
            >`
          : nothing}
      `;
    }
    return html`${renderKeysEnvBadge(this.envOverrides, this.envValues, [key])}${control}`;
  }

  private fieldErrors(relPath: string): string[] {
    if (this.saveError?.kind !== 'validation') return [];
    return fieldErrorMessages(this.saveError.fieldErrors, `${BRANDING_KOANF_PREFIX}.${relPath}`);
  }

  // --- Client-Side Validation ---

  private clientFieldErrors(): Record<string, string> {
    const errs: Record<string, string> = {};
    if (this.customFontMode && this.draft.font_sans.trim() === '' && !this.envLocked('font_sans')) {
      errs.font_sans = 'Enter a Google Fonts family name or select a preset.';
    } else {
      const fontErr = validateFontSansInput(this.draft.font_sans);
      if (fontErr && !this.envLocked('font_sans')) errs.font_sans = fontErr;
    }

    const accentErr = validateAccentColorInput(this.draft.accent_color);
    if (accentErr && !this.envLocked('accent_color')) errs.accent_color = accentErr;

    for (const slot of ASSET_SLOTS) {
      if (this.envLocked(slot.key)) continue;
      const val = getSlotValue(this.draft, slot);
      const urlErr = validateAssetUrlInput(val);
      if (urlErr) errs[slot.key] = urlErr;
    }

    if (totalDataUriBytes(this.draft) > MAX_BRAND_TOTAL_BYTES) {
      errs._totalBytes = 'Total embedded asset size exceeds the 512 KiB limit.';
    }
    return errs;
  }

  // --- Save / Discard / Reset ---

  private showSuccess(message: string): void {
    this.successMessage = message;
    if (this.successMessageTimer !== null) {
      clearTimeout(this.successMessageTimer);
    }
    this.successMessageTimer = setTimeout(() => {
      this.successMessage = null;
      this.successMessageTimer = null;
    }, 4000);
  }

  private handleDiscard(): void {
    this.draft = cloneBrandingFormState(this.baseline);
    this.customFontMode = !isCuratedOrBuiltinFont(this.baseline.font_sans);
    this.saveError = null;
    this.error = null;
    this.slotUploadErrors = {};
  }

  private async handleSave(): Promise<void> {
    if (!this.canEdit || this.saving) return;
    this.error = null;
    this.saveError = null;
    this.successMessage = null;

    const clientErrs = this.clientFieldErrors();
    if (Object.keys(clientErrs).length > 0) {
      return;
    }

    const diff = buildBrandingDiff(this.draft, this.baseline, this.envLockedSet());
    if (Object.keys(diff).length === 0) {
      this.showSuccess('No changes to save');
      return;
    }

    this.saving = true;
    try {
      const body: Record<string, unknown> = {
        server: { hub: { branding: diff } },
      };
      if (this.brandingRevision !== null) {
        body.expected_revisions = { branding: this.brandingRevision };
      }

      const res = await apiFetch('/api/v1/admin/server-config', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });

      if (!res.ok) {
        const err = await this.classifySaveError(res);
        if (err.kind === 'generic') {
          this.error = genericSaveErrorText(err, 'Failed to save branding settings');
        } else {
          this.saveError = err;
        }
        return;
      }

      invalidatePublicSettings();
      await getPublicSettings();
      await this.loadData(false);
      this.showSuccess('Branding settings saved and applied live');
    } catch {
      this.error = 'Failed to connect to server';
    } finally {
      this.saving = false;
    }
  }

  private canResetAll(): boolean {
    if (!this.canEdit || this.resetting || this.saving) return false;
    if (this.settingsTier === 'db') {
      return this.sectionMetadata?.origin === 'managed';
    }
    return hasAnyConfiguredBranding(this.baseline, this.hasPaletteOverrides);
  }

  private async handleConfirmResetAll(): Promise<void> {
    if (!this.canEdit || this.resetting) return;
    this.resetting = true;
    this.error = null;
    this.resetError = null;
    this.saveError = null;
    this.successMessage = null;

    try {
      let res: Response;
      if (this.settingsTier === 'file') {
        const clearPayload = buildFileModeResetPayload(
          this.envLockedSet(),
          this.hasPaletteOverrides
        );
        res = await apiFetch('/api/v1/admin/server-config', {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ server: { hub: { branding: clearPayload } } }),
        });
      } else {
        res = await apiFetch('/api/v1/admin/server-config/sections/branding', {
          method: 'DELETE',
        });
      }

      if (!res.ok) {
        this.resetError = await extractApiError(res, 'Failed to reset branding settings');
        return;
      }

      this.resetDialogOpen = false;
      this.resetError = null;
      invalidatePublicSettings();
      await getPublicSettings();
      await this.loadData(false);
      this.showSuccess('Branding reset to defaults');
    } catch {
      this.resetError = 'Failed to connect to server';
    } finally {
      this.resetting = false;
    }
  }

  // --- Field Mutations ---

  private updateField<K extends keyof BrandingFormState>(
    key: K,
    value: BrandingFormState[K]
  ): void {
    this.draft = { ...this.draft, [key]: value };
  }

  private updateSlot(slot: AssetSlotDef, value: string): void {
    this.draft = setSlotValue(this.draft, slot, value);
    if (this.slotUploadErrors[slot.key]) {
      const next = { ...this.slotUploadErrors };
      delete next[slot.key];
      this.slotUploadErrors = next;
    }
  }

  private handleFontSelectChange(value: string): void {
    if (value === CUSTOM_FONT_SENTINEL) {
      this.customFontMode = true;
      if (isCuratedOrBuiltinFont(this.draft.font_sans)) {
        this.updateField('font_sans', '');
      }
    } else {
      this.customFontMode = false;
      this.updateField('font_sans', value || 'default');
    }
  }

  private triggerFilePicker(slotKey: string): void {
    const input = this.shadowRoot?.querySelector<HTMLInputElement>(
      `input[type="file"][data-slot="${slotKey}"]`
    );
    if (input) {
      input.value = '';
      input.click();
    }
  }

  private async handleFileSelected(slot: AssetSlotDef, e: Event): Promise<void> {
    const input = e.target as HTMLInputElement;
    const file = input.files?.[0];
    if (!file) return;

    const currentSlotInfo = parseDataUriInfo(getSlotValue(this.draft, slot));
    const otherBytes = totalDataUriBytes(this.draft) - (currentSlotInfo?.decodedBytes ?? 0);
    const validationErr = validateAssetFile(file, slot, otherBytes);
    if (validationErr) {
      this.slotUploadErrors = { ...this.slotUploadErrors, [slot.key]: validationErr };
      return;
    }

    try {
      const rawDataUrl = await readFileAsDataUrl(file);
      const dataUrl = normalizeAssetDataUrl(rawDataUrl, file.name, slot);
      this.updateSlot(slot, dataUrl);
    } catch {
      this.slotUploadErrors = {
        ...this.slotUploadErrors,
        [slot.key]: 'Failed to read selected file.',
      };
    }
  }

  private async handleSwitchToUrl(slot: AssetSlotDef): Promise<void> {
    this.updateSlot(slot, '');
    await this.updateComplete;
    const input = this.shadowRoot?.querySelector<HTMLElement>(`sl-input[data-field="${slot.key}"]`);
    input?.focus();
  }

  // --- Render Helpers ---

  private renderPerKeyReset(label: string, visible: boolean, onReset: () => void) {
    if (!visible || !this.canEdit) return nothing;
    return html`
      <sl-tooltip content="Reset ${label} to default">
        <sl-icon-button
          class="reset-btn"
          name="arrow-counterclockwise"
          label="Reset ${label} to default"
          @click=${onReset}
        ></sl-icon-button>
      </sl-tooltip>
    `;
  }

  private renderCornerGlyph(kind: BrandCorners): TemplateResult {
    const rx = kind === 'square' ? 0 : kind === 'rounded' ? 6 : 3;
    return html`
      <svg class="corner-glyph" viewBox="0 0 16 16" aria-hidden="true">
        <rect
          x="2"
          y="2"
          width="12"
          height="12"
          rx=${rx}
          fill="none"
          stroke="currentColor"
          stroke-width="1.5"
        />
      </svg>
    `;
  }

  private renderAssetSlot(slot: AssetSlotDef, clientErrs: Record<string, string>): TemplateResult {
    const value = getSlotValue(this.draft, slot);
    const dataInfo = parseDataUriInfo(value);
    const locked = this.envLocked(slot.key);
    const uploadErr = this.slotUploadErrors[slot.key];
    const urlErr = clientErrs[slot.key];
    const serverErrs = this.fieldErrors(slot.key);
    const disabled = !this.canEdit || this.saving || locked;

    const previewSrc = value && !urlErr ? value : '';
    const preview =
      slot.group === 'logo'
        ? html`
            <div class="logo-swatch">
              <div class="logo-swatch-half sl-theme-light">
                ${previewSrc
                  ? html`<img
                      src=${previewSrc}
                      alt="${slot.label} on light"
                      referrerpolicy="no-referrer"
                    />`
                  : html`<sl-icon name="image" aria-hidden="true"></sl-icon>`}
              </div>
              <div class="logo-swatch-half sl-theme-dark">
                ${previewSrc
                  ? html`<img
                      src=${previewSrc}
                      alt="${slot.label} on dark"
                      referrerpolicy="no-referrer"
                    />`
                  : html`<sl-icon name="image" aria-hidden="true"></sl-icon>`}
              </div>
            </div>
          `
        : html`
            <div class="icon-swatch">
              ${previewSrc
                ? html`<img src=${previewSrc} alt="${slot.label}" referrerpolicy="no-referrer" />`
                : html`<sl-icon name="image" aria-hidden="true"></sl-icon>`}
            </div>
          `;

    const control = html`
      <input
        type="file"
        hidden
        data-slot=${slot.key}
        accept=${slot.accept}
        ?disabled=${disabled}
        @change=${(e: Event) => void this.handleFileSelected(slot, e)}
      />
      ${dataInfo
        ? html`
            <div class="data-uri-chip" data-data-uri-chip=${slot.key}>
              <span class="data-uri-meta">
                <sl-badge variant="primary" pill>Embedded</sl-badge>
                <span>${dataInfo.mediaType} · ${formatKiB(dataInfo.decodedBytes)}</span>
              </span>
              <span class="data-uri-actions">
                <sl-button
                  size="small"
                  variant="default"
                  ?disabled=${disabled}
                  @click=${() => this.triggerFilePicker(slot.key)}
                >
                  <sl-icon slot="prefix" name="upload"></sl-icon>
                  Replace <span class="sr-only">${slot.label} </span>file
                </sl-button>
                <sl-button
                  size="small"
                  variant="default"
                  ?disabled=${disabled}
                  @click=${() => void this.handleSwitchToUrl(slot)}
                >
                  <sl-icon slot="prefix" name="link"></sl-icon>
                  Switch <span class="sr-only">${slot.label} </span>to URL
                </sl-button>
                <sl-button
                  size="small"
                  variant="text"
                  ?disabled=${disabled}
                  @click=${() => this.updateSlot(slot, '')}
                >
                  Clear<span class="sr-only"> ${slot.label}</span>
                </sl-button>
              </span>
            </div>
          `
        : html`
            <div class="asset-input-row">
              <sl-input
                class="sr-control-label"
                label=${slot.label}
                data-field=${slot.key}
                .value=${value}
                placeholder="https://cdn.example.com/... or /path/to/asset"
                ?disabled=${disabled}
                @sl-input=${(e: Event) =>
                  this.updateSlot(slot, (e.target as HTMLInputElement).value)}
              ></sl-input>
              <sl-button
                size="medium"
                variant="default"
                data-upload-btn=${slot.key}
                ?disabled=${disabled}
                @click=${() => this.triggerFilePicker(slot.key)}
              >
                <sl-icon slot="prefix" name="upload"></sl-icon>
                Upload<span class="sr-only"> ${slot.label}</span>
              </sl-button>
            </div>
          `}
    `;

    return html`
      <div class="asset-row" data-asset-slot=${slot.key}>
        ${preview}
        <div class="asset-controls">
          <div class="field-label-row">
            <span class="field-label-text">
              ${slot.label}
              ${slot.dimensionHint
                ? html`<span class="dimension-hint">${slot.dimensionHint}</span>`
                : nothing}
            </span>
            ${this.renderPerKeyReset(slot.label, value !== '' && !locked, () =>
              this.updateSlot(slot, '')
            )}
          </div>
          ${this.renderFieldWithEnv(slot.key, value, control)}
          <span class="hint">${slot.hint}</span>
          ${uploadErr
            ? html`<span class="field-error" data-client-error=${slot.key}>${uploadErr}</span>`
            : nothing}
          ${urlErr
            ? html`<span class="field-error" data-client-error=${slot.key}>${urlErr}</span>`
            : nothing}
          ${renderFieldErrorMessages(serverErrs, slot.key)}
        </div>
      </div>
    `;
  }

  private renderContrastPairRow(pair: ContrastPairBadge): TemplateResult {
    const badgeVariant = !pair.enforced ? 'neutral' : pair.pass ? 'success' : 'danger';
    const statusText = !pair.enforced ? 'Stock' : pair.pass ? 'AA Pass' : 'AA Fail';
    const iconName = !pair.enforced
      ? 'info-circle'
      : pair.pass
        ? 'check-circle'
        : 'exclamation-triangle';
    return html`
      <div class="contrast-row" data-contrast-pair=${pair.id}>
        <span class="contrast-pair-meta">
          <span
            class="contrast-swatch-dot"
            style="--pair-fg: ${pair.fgHex}; --pair-bg: ${pair.bgHex}"
            aria-hidden="true"
            >Aa</span
          >
          <span>${pair.label}</span>
        </span>
        <sl-badge variant=${badgeVariant} pill data-contrast-status=${statusText.toLowerCase()}>
          <span class="contrast-badge-text">
            <sl-icon name=${iconName} aria-hidden="true"></sl-icon>
            ${statusText} · ${formatContrastRatio(pair.ratio)}
          </span>
        </sl-badge>
      </div>
    `;
  }

  private renderAccentContrastPanel(resolved: ResolvedBrandingPalette): TemplateResult {
    const primaryScale = resolved.scales.primary;
    const failedChecks = resolved.checks.filter((c) => !c.pass);

    return html`
      <div class="ramp-container" data-primary-ramp>
        <div class="ramp-header">
          <span>
            ${resolved.hasCustomPalette
              ? 'Generated Primary Scale (OKLCH 50–950)'
              : 'Stock Primary Scale (50–950)'}
          </span>
          <span>
            Light primary: <code>${resolved.light.primary}</code> · Dark primary:
            <code>${resolved.dark.primary}</code>
          </span>
        </div>
        <div class="ramp-strip">
          ${SCALE_STEPS.map((step) => {
            const hexVal = primaryScale[step];
            return html`
              <div class="ramp-swatch" data-ramp-step=${step} title="primary-${step}: ${hexVal}">
                <span class="ramp-swatch-chip" style="--swatch-color: ${hexVal}"></span>
                <span class="ramp-swatch-step">${step}</span>
                <span class="sr-only">${hexVal}</span>
              </div>
            `;
          })}
        </div>
      </div>

      <div class="contrast-section" data-accent-contrast-badges>
        <div class="contrast-group" data-contrast-group="light">
          <span class="contrast-group-title">
            <sl-icon name="sun" aria-hidden="true"></sl-icon>
            Light Mode Contrast (min 4.50:1)
          </span>
          <div class="contrast-badge-list">
            ${resolved.keyPairs.light.map((pair) => this.renderContrastPairRow(pair))}
          </div>
        </div>
        <div class="contrast-group" data-contrast-group="dark">
          <span class="contrast-group-title">
            <sl-icon name="moon" aria-hidden="true"></sl-icon>
            Dark Mode Contrast (min 4.50:1)
          </span>
          <div class="contrast-badge-list">
            ${resolved.keyPairs.dark.map((pair) => this.renderContrastPairRow(pair))}
          </div>
        </div>
      </div>

      ${resolved.hasCustomPalette && failedChecks.length > 0
        ? html`
            <div class="contrast-advisory" data-contrast-advisory>
              <div class="contrast-advisory-header">
                <sl-icon name="exclamation-triangle" aria-hidden="true"></sl-icon>
                <span>
                  <strong>WCAG 2.1 AA contrast warning (advisory):</strong>
                  ${failedChecks.length} ${failedChecks.length === 1 ? 'pair is' : 'pairs are'}
                  below the minimum contrast threshold. Server validation enforces WCAG 2.1 AA on
                  save.
                </span>
              </div>
              <ul>
                ${failedChecks
                  .slice(0, 5)
                  .map((c) => html`<li><code>${c.path}</code>: ${c.message}</li>`)}
              </ul>
            </div>
          `
        : nothing}
    `;
  }

  // --- Main Render ---

  override render() {
    if (this.loading) {
      return html`
        <div class="loading-container">
          <sl-spinner style="font-size: 2rem;"></sl-spinner>
        </div>
      `;
    }

    const lockedSet = this.envLockedSet();
    const dirty = isBrandingDirty(this.draft, this.baseline, lockedSet);
    const clientErrs = this.clientFieldErrors();
    const hasClientErrs = Object.keys(clientErrs).length > 0;
    const totalBytes = totalDataUriBytes(this.draft);
    const budgetPct = Math.min(100, Math.round((totalBytes / MAX_BRAND_TOTAL_BYTES) * 100));
    const budgetClass =
      totalBytes > MAX_BRAND_TOTAL_BYTES ? 'over-limit' : budgetPct >= 80 ? 'near-limit' : '';

    const resolvedPalette = resolveBrandingPalette({
      accentColor: this.draft.accent_color,
      palette: this.rawPalette,
    });

    const fontSelectValue = this.customFontMode
      ? CUSTOM_FONT_SENTINEL
      : isCuratedOrBuiltinFont(this.draft.font_sans)
        ? this.draft.font_sans || 'default'
        : CUSTOM_FONT_SENTINEL;

    const accentPickerVal = HEX_COLOR_RE.test(this.draft.accent_color.trim())
      ? this.draft.accent_color.trim().toLowerCase()
      : DEFAULT_ACCENT_PICKER_COLOR;

    const logoSlots = ASSET_SLOTS.filter((s) => s.group === 'logo');
    const iconSlots = ASSET_SLOTS.filter((s) => s.group === 'icons');

    return html`
      <div class="header">
        <div class="header-left">
          <sl-icon name="palette"></sl-icon>
          <h1>Branding</h1>
        </div>
        <sl-button
          variant="danger"
          outline
          size="small"
          data-reset-all-btn
          ?disabled=${!this.canResetAll()}
          @click=${() => {
            this.resetError = null;
            this.resetDialogOpen = true;
          }}
        >
          <sl-icon slot="prefix" name="arrow-counterclockwise"></sl-icon>
          Reset All Branding
        </sl-button>
      </div>

      <p class="header-description">
        Customize Hub-wide product identity, corners, typography, logos, icons, and accent color.
        Changes apply live without restarting the Hub.
      </p>

      ${this.sectionMetadata
        ? html`
            <div class="provenance-caption" data-provenance>
              <sl-badge variant="neutral" pill>
                ${this.sectionMetadata.origin === 'managed'
                  ? 'Managed in database'
                  : this.sectionMetadata.source === 'db'
                    ? 'Seeded defaults'
                    : this.settingsTier === 'file'
                      ? 'File-backed (settings.yaml)'
                      : 'Stock defaults'}
              </sl-badge>
              ${typeof this.sectionMetadata.revision === 'number' &&
              this.sectionMetadata.revision > 0
                ? html`<span>Revision ${this.sectionMetadata.revision}</span>`
                : nothing}
            </div>
          `
        : nothing}
      ${this.permissionsLoaded && !this.canEdit
        ? html`
            <sl-alert class="readonly-banner" variant="neutral" open data-readonly-banner>
              <sl-icon slot="icon" name="info-circle"></sl-icon>
              You have read-only access to branding settings. Updating branding requires the
              <code>hub.config.update</code> permission.
            </sl-alert>
          `
        : nothing}
      ${this.saveError?.kind === 'conflict'
        ? renderConflictNotice(this.saveError, () => {
            this.saveError = null;
            void this.loadData();
          })
        : nothing}
      ${this.saveError?.kind === 'layer0' ? renderLayer0Notice(this.saveError) : nothing}
      ${this.saveError?.kind === 'validation'
        ? renderSectionErrorSummary(this.saveError.fieldErrors, 'branding')
        : nothing}
      ${this.error
        ? html`<div class="status-message error" role="alert">${this.error}</div>`
        : nothing}
      ${this.successMessage
        ? html`<div class="status-message success" role="status">${this.successMessage}</div>`
        : nothing}

      <!-- Scoped Light & Dark Live Preview -->
      <scion-branding-preview
        .draft=${this.draft}
        .resolved=${resolvedPalette}
      ></scion-branding-preview>

      <!-- Group 1: Identity & Attribution -->
      <section class="section" aria-labelledby="branding-identity-heading">
        <div class="section-header">
          <h2 id="branding-identity-heading" class="section-title">Identity &amp; Attribution</h2>
        </div>

        <div class="form-grid">
          <div class="form-field">
            <div class="field-label-row">
              <span class="field-label-text">Product Name</span>
              ${this.renderPerKeyReset(
                'Product Name',
                this.draft.name !== '' && !this.envLocked('name'),
                () => this.updateField('name', '')
              )}
            </div>
            ${this.renderFieldWithEnv(
              'name',
              this.draft.name,
              html`
                <sl-input
                  class="sr-control-label"
                  label="Product Name"
                  data-field="name"
                  .value=${this.draft.name}
                  placeholder="Scion"
                  maxlength=${MAX_BRAND_NAME_RUNES}
                  ?disabled=${!this.canEdit || this.saving}
                  @sl-input=${(e: Event) =>
                    this.updateField('name', (e.target as HTMLInputElement).value)}
                ></sl-input>
              `
            )}
            <span class="hint"
              >1–64 characters. Shown in the sidebar, header, and browser tab title.</span
            >
            ${renderFieldErrorMessages(this.fieldErrors('name'), 'name')}
          </div>

          <div class="form-field">
            <div class="field-label-row">
              <span class="field-label-text">Short Name</span>
              ${this.renderPerKeyReset(
                'Short Name',
                this.draft.short_name !== '' && !this.envLocked('short_name'),
                () => this.updateField('short_name', '')
              )}
            </div>
            ${this.renderFieldWithEnv(
              'short_name',
              this.draft.short_name,
              html`
                <sl-input
                  class="sr-control-label"
                  label="Short Name"
                  data-field="short_name"
                  .value=${this.draft.short_name}
                  placeholder=${this.draft.name || 'Scion'}
                  maxlength=${MAX_BRAND_SHORT_NAME_RUNES}
                  ?disabled=${!this.canEdit || this.saving}
                  @sl-input=${(e: Event) =>
                    this.updateField('short_name', (e.target as HTMLInputElement).value)}
                ></sl-input>
              `
            )}
            <span class="hint"
              >1–24 characters. Used in the PWA manifest; defaults to Product Name.</span
            >
            ${renderFieldErrorMessages(this.fieldErrors('short_name'), 'short_name')}
          </div>

          <div class="form-field full-width">
            <div class="field-label-row">
              <span class="field-label-text">Subtitle</span>
              ${this.renderPerKeyReset(
                'Subtitle',
                this.draft.subtitle !== '' && !this.envLocked('subtitle'),
                () => this.updateField('subtitle', '')
              )}
            </div>
            ${this.renderFieldWithEnv(
              'subtitle',
              this.draft.subtitle,
              html`
                <sl-input
                  class="sr-control-label"
                  label="Subtitle"
                  data-field="subtitle"
                  .value=${this.draft.subtitle}
                  placeholder="Optional tagline"
                  maxlength=${MAX_BRAND_SUBTITLE_RUNES}
                  ?disabled=${!this.canEdit || this.saving}
                  @sl-input=${(e: Event) =>
                    this.updateField('subtitle', (e.target as HTMLInputElement).value)}
                ></sl-input>
              `
            )}
            <span class="hint">
              Up to 80 characters. The stock tagline is hidden when Product Name is customized and
              Subtitle is empty.
            </span>
            ${renderFieldErrorMessages(this.fieldErrors('subtitle'), 'subtitle')}
          </div>

          <div class="form-field full-width">
            <div class="field-label-row">
              <span class="field-label-text">Scion Attribution</span>
              ${this.renderPerKeyReset(
                'Scion Attribution',
                this.draft.attribution !== 'shown' && !this.envLocked('attribution'),
                () => this.updateField('attribution', 'shown')
              )}
            </div>
            ${this.renderFieldWithEnv(
              'attribution',
              this.draft.attribution,
              html`
                <sl-radio-group
                  class="sr-control-label"
                  label="Scion Attribution"
                  data-field="attribution"
                  .value=${this.draft.attribution}
                  @sl-change=${(e: Event) =>
                    this.updateField(
                      'attribution',
                      (e.target as HTMLInputElement).value === 'hidden' ? 'hidden' : 'shown'
                    )}
                >
                  <sl-radio-button value="shown" ?disabled=${!this.canEdit || this.saving}>
                    Shown
                  </sl-radio-button>
                  <sl-radio-button value="hidden" ?disabled=${!this.canEdit || this.saving}>
                    Hidden
                  </sl-radio-button>
                </sl-radio-group>
              `
            )}
            <span class="hint">
              Shown on login, invite, status pages, and expanded sidebar footers when custom
              branding is active.
            </span>
            ${renderFieldErrorMessages(this.fieldErrors('attribution'), 'attribution')}
          </div>
        </div>
      </section>

      <!-- Group 2: Shape & Typography -->
      <section class="section" aria-labelledby="branding-shape-heading">
        <div class="section-header">
          <h2 id="branding-shape-heading" class="section-title">Shape &amp; Typography</h2>
        </div>

        <div class="form-grid">
          <div class="form-field">
            <div class="field-label-row">
              <span class="field-label-text">Corner Style</span>
              ${this.renderPerKeyReset(
                'Corner Style',
                this.draft.corners !== 'default' && !this.envLocked('corners'),
                () => this.updateField('corners', 'default')
              )}
            </div>
            ${this.renderFieldWithEnv(
              'corners',
              this.draft.corners,
              html`
                <sl-radio-group
                  class="sr-control-label"
                  label="Corner Style"
                  data-field="corners"
                  .value=${this.draft.corners}
                  @sl-change=${(e: Event) => {
                    const v = (e.target as HTMLInputElement).value;
                    this.updateField('corners', v === 'square' || v === 'rounded' ? v : 'default');
                  }}
                >
                  <sl-radio-button value="default" ?disabled=${!this.canEdit || this.saving}>
                    <span class="corner-option">
                      ${this.renderCornerGlyph('default')} Default
                    </span>
                  </sl-radio-button>
                  <sl-radio-button value="square" ?disabled=${!this.canEdit || this.saving}>
                    <span class="corner-option"> ${this.renderCornerGlyph('square')} Square </span>
                  </sl-radio-button>
                  <sl-radio-button value="rounded" ?disabled=${!this.canEdit || this.saving}>
                    <span class="corner-option">
                      ${this.renderCornerGlyph('rounded')} Rounded
                    </span>
                  </sl-radio-button>
                </sl-radio-group>
              `
            )}
            <span class="hint">
              Adjusts cards, buttons, inputs, and dialogs. Pill badges and circular avatars remain
              rounded.
            </span>
            ${renderFieldErrorMessages(this.fieldErrors('corners'), 'corners')}
          </div>

          <div class="form-field">
            <div class="field-label-row">
              <span class="field-label-text">Sans-Serif Font</span>
              ${this.renderPerKeyReset(
                'Sans-Serif Font',
                this.draft.font_sans !== 'default' &&
                  this.draft.font_sans !== '' &&
                  !this.envLocked('font_sans'),
                () => {
                  this.customFontMode = false;
                  this.updateField('font_sans', 'default');
                }
              )}
            </div>
            ${this.renderFieldWithEnv(
              'font_sans',
              this.draft.font_sans,
              html`
                <sl-select
                  class="sr-control-label"
                  label="Sans-Serif Font"
                  data-field="font_sans_select"
                  .value=${fontSelectValue}
                  hoist
                  ?disabled=${!this.canEdit || this.saving}
                  @sl-change=${(e: Event) =>
                    this.handleFontSelectChange((e.target as HTMLSelectElement).value)}
                >
                  ${FONT_SANS_OPTIONS.map(
                    (opt) => html`<sl-option value=${opt.value}>${opt.label}</sl-option>`
                  )}
                  <sl-option value=${CUSTOM_FONT_SENTINEL}>Custom Google Font…</sl-option>
                </sl-select>
                ${this.customFontMode
                  ? html`
                      <sl-input
                        class="sr-control-label custom-font-input"
                        label="Custom Google Font family"
                        data-field="font_sans_custom"
                        .value=${this.draft.font_sans}
                        placeholder="e.g. Lato, Open Sans, Work Sans"
                        maxlength="64"
                        ?disabled=${!this.canEdit || this.saving}
                        @sl-input=${(e: Event) =>
                          this.updateField('font_sans', (e.target as HTMLInputElement).value)}
                      ></sl-input>
                    `
                  : nothing}
              `
            )}
            <span class="hint">
              Use "System OS Sans" for air-gapped environments (no external CDN). Monospace
              (JetBrains Mono) stays fixed.
            </span>
            ${clientErrs.font_sans
              ? html`<span class="field-error" data-client-error="font_sans"
                  >${clientErrs.font_sans}</span
                >`
              : nothing}
            ${renderFieldErrorMessages(this.fieldErrors('font_sans'), 'font_sans')}
          </div>
        </div>
      </section>

      <!-- Group 3: Logos & Icons -->
      <section class="section" aria-labelledby="branding-assets-heading">
        <div class="section-header">
          <h2 id="branding-assets-heading" class="section-title">Logos &amp; Icons</h2>
        </div>

        <div class="budget-meter ${budgetClass}" data-asset-budget>
          <div class="budget-meter-header">
            <span>Embedded asset budget (<code>data:</code> uploads)</span>
            <strong>${formatKiB(totalBytes)} / 512.0 KiB</strong>
          </div>
          <sl-progress-bar
            value=${budgetPct}
            label="Embedded asset storage budget"
          ></sl-progress-bar>
          <span class="hint">
            Max 128 KiB per uploaded file, 512 KiB total across all slots. External
            <code>https://</code> and same-origin <code>/</code> paths do not count toward this
            budget.
          </span>
          ${clientErrs._totalBytes
            ? html`<span class="field-error">${clientErrs._totalBytes}</span>`
            : nothing}
        </div>

        <div class="asset-subsection-title">Logos</div>
        <div class="asset-list">
          ${logoSlots.map((slot) => this.renderAssetSlot(slot, clientErrs))}
        </div>

        <div class="asset-subsection-title">Favicon &amp; PWA Icons</div>
        <div class="asset-list">
          ${iconSlots.map((slot) => this.renderAssetSlot(slot, clientErrs))}
        </div>
      </section>

      <!-- Group 4: Colors -->
      <section class="section" aria-labelledby="branding-colors-heading">
        <div class="section-header">
          <h2 id="branding-colors-heading" class="section-title">Colors</h2>
        </div>

        <div class="form-field">
          <div class="field-label-row">
            <span class="field-label-text">Accent Color</span>
            ${this.renderPerKeyReset(
              'Accent Color',
              this.draft.accent_color !== '' && !this.envLocked('accent_color'),
              () => this.updateField('accent_color', '')
            )}
          </div>
          ${this.renderFieldWithEnv(
            'accent_color',
            this.draft.accent_color,
            html`
              <div class="color-input-row">
                <input
                  type="color"
                  class="color-picker-native"
                  aria-label="Accent color picker"
                  .value=${accentPickerVal}
                  ?disabled=${!this.canEdit || this.saving}
                  @input=${(e: Event) =>
                    this.updateField('accent_color', (e.target as HTMLInputElement).value)}
                />
                <sl-input
                  class="sr-control-label"
                  label="Accent Color"
                  data-field="accent_color"
                  .value=${this.draft.accent_color}
                  placeholder=${DEFAULT_ACCENT_PICKER_COLOR}
                  maxlength="7"
                  ?disabled=${!this.canEdit || this.saving}
                  @sl-input=${(e: Event) =>
                    this.updateField('accent_color', (e.target as HTMLInputElement).value)}
                ></sl-input>
              </div>
            `
          )}
          <span class="hint">
            Hex color in <code>#RRGGBB</code> format. Generates a WCAG 2.1 AA compliant primary
            color ramp for both light and dark modes on save.
          </span>
          ${clientErrs.accent_color
            ? html`<span class="field-error" data-client-error="accent_color"
                >${clientErrs.accent_color}</span
              >`
            : nothing}
          ${renderFieldErrorMessages(this.fieldErrors('accent_color'), 'accent_color')}
        </div>

        ${this.renderAccentContrastPanel(resolvedPalette)}
        ${this.hasPaletteOverrides
          ? html`
              <sl-alert class="palette-notice" variant="primary" open data-palette-notice>
                <sl-icon slot="icon" name="info-circle"></sl-icon>
                Custom palette overrides configured via CLI/YAML are active and will be preserved
                when saving other settings.
              </sl-alert>
            `
          : nothing}
      </section>

      <!-- Sticky Save Bar -->
      ${this.canEdit
        ? html`
            <div class="sticky-save-bar" data-save-bar>
              <div class="sticky-save-status">
                ${dirty
                  ? html`<sl-badge variant="warning" pill>Unsaved changes</sl-badge>
                      <span>Save to apply branding live across the Hub.</span>`
                  : html`<span>All changes saved.</span>`}
              </div>
              <div class="sticky-save-actions">
                <sl-button
                  variant="default"
                  size="small"
                  data-discard-btn
                  ?disabled=${!dirty || this.saving}
                  @click=${() => this.handleDiscard()}
                >
                  Discard changes
                </sl-button>
                <sl-button
                  variant="primary"
                  size="small"
                  data-save-btn
                  ?loading=${this.saving}
                  ?disabled=${!dirty || this.saving || hasClientErrs}
                  @click=${() => void this.handleSave()}
                >
                  Save Branding
                </sl-button>
              </div>
            </div>
          `
        : nothing}

      <!-- Reset All Branding Confirmation Dialog -->
      <sl-dialog
        label="Reset All Branding"
        data-reset-dialog
        ?open=${this.resetDialogOpen}
        @sl-request-close=${(e: Event) => {
          if (this.resetting) {
            e.preventDefault();
            return;
          }
          this.resetDialogOpen = false;
          this.resetError = null;
        }}
      >
        <p>
          ${this.settingsTier === 'file'
            ? 'This clears all editable branding settings in settings.yaml and restores stock Scion defaults immediately.'
            : 'This removes managed branding overrides from the database and reverts to deployment/bootstrap defaults immediately.'}
        </p>
        ${this.resetError
          ? html`<div class="status-message error" role="alert" data-reset-error>
              ${this.resetError}
            </div>`
          : nothing}
        <div class="dialog-footer" slot="footer">
          <sl-button
            variant="default"
            ?disabled=${this.resetting}
            @click=${() => {
              this.resetDialogOpen = false;
              this.resetError = null;
            }}
          >
            Cancel
          </sl-button>
          <sl-button
            variant="danger"
            data-confirm-reset-btn
            ?loading=${this.resetting}
            @click=${() => void this.handleConfirmResetAll()}
          >
            Reset Branding
          </sl-button>
        </div>
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-admin-branding': ScionPageAdminBranding;
  }
}
