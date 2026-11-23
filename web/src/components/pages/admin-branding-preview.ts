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
 * Scoped Light & Dark Live Preview Card (`<scion-branding-preview>`) for
 * `/admin/branding`.
 *
 * Scopes all preview tokens via inline CSS custom properties on each preview
 * pane element only — never mutates `document.documentElement` or live Hub
 * branding until the admin clicks Save.
 *
 * Accessibility:
 * - No `aria-live` region on the preview panes or contrast summary so dragging
 *   a color picker does not trigger noisy screen-reader announcements.
 * - Contrast status badges always carry explicit text + icon (never color alone).
 * - Decorative mock controls inside the preview panes are non-interactive so
 *   keyboard focus is never trapped in fake buttons or inputs.
 */

import { LitElement, html, css, nothing, type TemplateResult } from 'lit';
import { customElement, property } from 'lit/decorators.js';

import { DEFAULT_BRAND_SUBTITLE } from '../../client/branding.js';
import {
  defaultBrandingFormState,
  validateAssetUrlInput,
  validateFontSansInput,
  type BrandCorners,
  type BrandingFormState,
} from './admin-branding-state.js';
import {
  compositeAlphaHex,
  resolveBrandingPalette,
  type BrandingSemanticTokens,
  type ResolvedBrandingPalette,
} from './admin-branding-contrast.js';

const SYSTEM_FONT_SANS_FALLBACK =
  "ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, 'Helvetica Neue', Arial, sans-serif";
const DEFAULT_FONT_SANS_STACK = `'Inter', ${SYSTEM_FONT_SANS_FALLBACK}`;

const CURATED_FONT_FAMILIES: Readonly<Record<string, string>> = {
  'ibm-plex-sans': 'IBM Plex Sans',
  'plus-jakarta-sans': 'Plus Jakarta Sans',
  'dm-sans': 'DM Sans',
  'source-sans-3': 'Source Sans 3',
};

/** Resolves a `font_sans` draft value to a safe CSS `font-family` stack. */
export function resolvePreviewFontStack(fontSans: string): string {
  const trimmed = fontSans.trim();
  if (!trimmed || trimmed === 'default') return DEFAULT_FONT_SANS_STACK;
  if (trimmed === 'system') return SYSTEM_FONT_SANS_FALLBACK;
  if (validateFontSansInput(trimmed) !== null) return DEFAULT_FONT_SANS_STACK;
  const normalized = trimmed.replace(/\s+/g, ' ');
  const family = CURATED_FONT_FAMILIES[normalized] ?? normalized;
  return `'${family}', ${SYSTEM_FONT_SANS_FALLBACK}`;
}

/** Resolves a `corners` preset to preview radius token values matching `branding_palette.go`. */
export function resolvePreviewRadii(corners: BrandCorners): {
  sm: string;
  base: string;
  lg: string;
} {
  switch (corners) {
    case 'square':
      return { sm: '0', base: '0', lg: '0' };
    case 'rounded':
      return { sm: '0.375rem', base: '0.75rem', lg: '1rem' };
    default:
      return { sm: '0.25rem', base: '0.5rem', lg: '0.75rem' };
  }
}

@customElement('scion-branding-preview')
export class ScionBrandingPreview extends LitElement {
  @property({ attribute: false })
  draft: BrandingFormState = defaultBrandingFormState();

  @property({ attribute: false })
  resolved: ResolvedBrandingPalette = resolveBrandingPalette({});

  static override styles = css`
    :host {
      display: block;
      margin-bottom: 1.5rem;
    }

    .preview-card {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.25rem 1.5rem;
    }

    .preview-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 0.75rem;
      margin-bottom: 1rem;
      padding-bottom: 0.75rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      flex-wrap: wrap;
    }

    .preview-title-group {
      display: flex;
      flex-direction: column;
      gap: 0.125rem;
    }

    .preview-title {
      font-size: 1rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }

    .preview-caption {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }

    .summary-badge-content {
      display: inline-flex;
      align-items: center;
      gap: 0.375rem;
    }

    .preview-grid {
      display: grid;
      grid-template-columns: 1fr 1fr;
      gap: 1rem;
    }

    @media (max-width: 768px) {
      .preview-grid {
        grid-template-columns: 1fr;
      }
    }

    .preview-pane-wrapper {
      display: flex;
      flex-direction: column;
      gap: 0.375rem;
      min-width: 0;
    }

    .pane-label-row {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 0.5rem;
      font-size: 0.75rem;
      font-weight: 600;
      color: var(--scion-text-secondary, #475569);
    }

    .pane-label-left {
      display: inline-flex;
      align-items: center;
      gap: 0.375rem;
    }

    .preview-pane {
      background: var(--preview-bg, var(--scion-bg, #f8fafc));
      color: var(--preview-text, var(--scion-text, #1e293b));
      border: 1px solid var(--preview-border, var(--scion-border, #e2e8f0));
      border-radius: var(--preview-radius-lg, var(--scion-radius-lg, 0.75rem));
      font-family: var(--preview-font-sans, var(--scion-font-sans, sans-serif));
      overflow: hidden;
      display: flex;
      flex-direction: column;
      min-width: 0;
    }

    .mock-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 0.5rem;
      padding: 0.625rem 0.875rem;
      background: var(--preview-surface, var(--scion-surface, #ffffff));
      border-bottom: 1px solid var(--preview-border, var(--scion-border, #e2e8f0));
      min-width: 0;
    }

    .mock-brand {
      display: inline-flex;
      align-items: center;
      gap: 0.5rem;
      min-width: 0;
    }

    .mock-logo {
      height: 1.375rem;
      max-width: 6rem;
      object-fit: contain;
      flex-shrink: 0;
    }

    .mock-monogram {
      width: 1.5rem;
      height: 1.5rem;
      border-radius: var(--preview-radius-sm, var(--scion-radius-sm, 0.25rem));
      background: var(--preview-primary, var(--scion-primary, #3b82f6));
      color: var(--preview-primary-text, var(--scion-primary-text, #ffffff));
      display: inline-flex;
      align-items: center;
      justify-content: center;
      font-size: 0.75rem;
      font-weight: 700;
      flex-shrink: 0;
    }

    .mock-brand-text {
      display: flex;
      flex-direction: column;
      min-width: 0;
    }

    .mock-brand-name {
      font-size: 0.875rem;
      font-weight: 700;
      color: var(--preview-text, var(--scion-text, #1e293b));
      white-space: nowrap;
      overflow: hidden;
      text-overflow: ellipsis;
    }

    .mock-brand-subtitle {
      font-size: 0.6875rem;
      color: var(--preview-text-muted, var(--scion-text-muted, #64748b));
      white-space: nowrap;
      overflow: hidden;
      text-overflow: ellipsis;
    }

    .mock-status-pill {
      display: inline-flex;
      align-items: center;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      font-size: 0.6875rem;
      font-weight: 600;
      background: var(--preview-badge-bg, var(--scion-badge-primary-bg, #dbeafe));
      color: var(--preview-badge-text, var(--scion-badge-primary-text, #1e40af));
      white-space: nowrap;
      flex-shrink: 0;
    }

    .mock-body {
      padding: 0.875rem;
      display: flex;
      flex-direction: column;
      gap: 0.625rem;
    }

    .mock-surface-card {
      background: var(--preview-surface, var(--scion-surface, #ffffff));
      border: 1px solid var(--preview-border, var(--scion-border, #e2e8f0));
      border-radius: var(--preview-radius, var(--scion-radius, 0.5rem));
      padding: 0.75rem;
      display: flex;
      flex-direction: column;
      gap: 0.5rem;
    }

    .mock-heading {
      font-size: 0.875rem;
      font-weight: 600;
      color: var(--preview-text, var(--scion-text, #1e293b));
      margin: 0;
    }

    .mock-copy {
      font-size: 0.75rem;
      line-height: 1.4;
      color: var(--preview-text-secondary, var(--scion-text-secondary, #475569));
      margin: 0;
    }

    .mock-controls {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      flex-wrap: wrap;
    }

    .mock-input {
      flex: 1;
      min-width: 7rem;
      padding: 0.3125rem 0.625rem;
      font-size: 0.75rem;
      color: var(--preview-text-muted, var(--scion-text-muted, #64748b));
      background: var(--preview-bg-subtle, var(--scion-bg-subtle, #f1f5f9));
      border: 1px solid var(--preview-border, var(--scion-border, #e2e8f0));
      border-radius: var(--preview-radius, var(--scion-radius, 0.5rem));
    }

    .mock-primary-btn {
      display: inline-flex;
      align-items: center;
      justify-content: center;
      padding: 0.3125rem 0.75rem;
      font-size: 0.75rem;
      font-weight: 600;
      background: var(--preview-primary, var(--scion-primary, #3b82f6));
      color: var(--preview-primary-text, var(--scion-primary-text, #ffffff));
      border-radius: var(--preview-radius, var(--scion-radius, 0.5rem));
      white-space: nowrap;
    }

    .mock-link {
      font-size: 0.75rem;
      font-weight: 500;
      color: var(--preview-primary, var(--scion-primary, #3b82f6));
      text-decoration: underline;
    }

    .mock-footer {
      padding: 0.4375rem 0.875rem;
      border-top: 1px solid var(--preview-border, var(--scion-border, #e2e8f0));
      background: var(--preview-bg-subtle, var(--scion-bg-subtle, #f1f5f9));
      font-size: 0.6875rem;
      color: var(--preview-text-muted, var(--scion-text-muted, #64748b));
      text-align: center;
    }
  `;

  private validLogoSrc(raw: string): string {
    const trimmed = raw.trim();
    if (!trimmed) return '';
    if (validateAssetUrlInput(trimmed) !== null) return '';
    return trimmed;
  }

  private paneStyleString(mode: 'light' | 'dark', tokens: BrandingSemanticTokens): string {
    const radii = resolvePreviewRadii(this.draft.corners);
    const fontStack = resolvePreviewFontStack(this.draft.font_sans);
    const primaryScale = this.resolved.scales.primary;
    const badgeBg =
      mode === 'light'
        ? primaryScale['100']
        : compositeAlphaHex(primaryScale['500'], 0.2, tokens.surface);
    const badgeText = mode === 'light' ? primaryScale['800'] : primaryScale['300'];

    return [
      `--preview-bg: ${tokens.bg}`,
      `--preview-bg-subtle: ${tokens.bg_subtle}`,
      `--preview-surface: ${tokens.surface}`,
      `--preview-surface-raised: ${tokens.surface_raised}`,
      `--preview-text: ${tokens.text}`,
      `--preview-text-secondary: ${tokens.text_secondary}`,
      `--preview-text-muted: ${tokens.text_muted}`,
      `--preview-border: ${tokens.border}`,
      `--preview-primary: ${tokens.primary}`,
      `--preview-primary-hover: ${tokens.primary_hover}`,
      `--preview-primary-active: ${tokens.primary_active}`,
      `--preview-primary-text: ${tokens.primary_text}`,
      `--preview-badge-bg: ${badgeBg}`,
      `--preview-badge-text: ${badgeText}`,
      `--preview-radius-sm: ${radii.sm}`,
      `--preview-radius: ${radii.base}`,
      `--preview-radius-lg: ${radii.lg}`,
      `--preview-font-sans: ${fontStack}`,
    ].join('; ');
  }

  private renderSummaryBadge(): TemplateResult {
    if (!this.resolved.hasCustomPalette) {
      return html`
        <sl-badge variant="neutral" pill data-preview-contrast-summary="stock">
          <span class="summary-badge-content">
            <sl-icon name="info-circle" aria-hidden="true"></sl-icon>
            Stock palette defaults
          </span>
        </sl-badge>
      `;
    }
    if (this.resolved.syntaxErrors.length > 0) {
      return html`
        <sl-badge variant="warning" pill data-preview-contrast-summary="invalid">
          <span class="summary-badge-content">
            <sl-icon name="exclamation-triangle" aria-hidden="true"></sl-icon>
            Invalid color format
          </span>
        </sl-badge>
      `;
    }
    if (this.resolved.summary.allPass) {
      return html`
        <sl-badge variant="success" pill data-preview-contrast-summary="pass">
          <span class="summary-badge-content">
            <sl-icon name="check-circle" aria-hidden="true"></sl-icon>
            WCAG AA Pass (${this.resolved.summary.passed}/${this.resolved.summary.total})
          </span>
        </sl-badge>
      `;
    }
    const n = this.resolved.summary.failed;
    return html`
      <sl-badge variant="danger" pill data-preview-contrast-summary="fail">
        <span class="summary-badge-content">
          <sl-icon name="exclamation-triangle" aria-hidden="true"></sl-icon>
          WCAG AA Fail (${n} ${n === 1 ? 'issue' : 'issues'})
        </span>
      </sl-badge>
    `;
  }

  private renderPane(mode: 'light' | 'dark'): TemplateResult {
    const tokens = mode === 'light' ? this.resolved.light : this.resolved.dark;
    const brandName = this.draft.name.trim() || 'Scion';
    const customName = brandName !== 'Scion';
    const rawSubtitle = this.draft.subtitle.trim();
    const subtitle = rawSubtitle || (customName ? '' : DEFAULT_BRAND_SUBTITLE);

    const lightLogo = this.validLogoSrc(this.draft.logo.light);
    const darkLogo = this.validLogoSrc(this.draft.logo.dark);
    const logoSrc = mode === 'light' ? lightLogo || darkLogo : darkLogo || lightLogo;
    const isCustomBrand = customName || Boolean(lightLogo) || Boolean(darkLogo);
    const showAttribution = isCustomBrand && this.draft.attribution !== 'hidden';
    const monogram = (Array.from(brandName)[0] ?? 'S').toUpperCase();

    return html`
      <div class="preview-pane-wrapper">
        <div class="pane-label-row">
          <span class="pane-label-left">
            <sl-icon name=${mode === 'light' ? 'sun' : 'moon'} aria-hidden="true"></sl-icon>
            ${mode === 'light' ? 'Light Mode' : 'Dark Mode'}
          </span>
        </div>
        <div
          class="preview-pane"
          data-preview-pane=${mode}
          style=${this.paneStyleString(mode, tokens)}
        >
          <div class="mock-header">
            <div class="mock-brand">
              ${logoSrc
                ? html`<img
                    class="mock-logo"
                    src=${logoSrc}
                    alt="${brandName} ${mode} logo preview"
                    referrerpolicy="no-referrer"
                  />`
                : html`<span class="mock-monogram" aria-hidden="true">${monogram}</span>`}
              <div class="mock-brand-text">
                <span class="mock-brand-name" data-preview-brand-name>${brandName}</span>
                ${subtitle
                  ? html`<span class="mock-brand-subtitle" data-preview-subtitle>${subtitle}</span>`
                  : nothing}
              </div>
            </div>
            <span class="mock-status-pill" aria-hidden="true">Running</span>
          </div>

          <div class="mock-body" aria-hidden="true">
            <div class="mock-surface-card">
              <p class="mock-heading">Active Agents</p>
              <p class="mock-copy">
                Preview of body typography, surface contrast, corners, and primary actions.
              </p>
              <div class="mock-controls">
                <span class="mock-input">Filter agents…</span>
                <span class="mock-primary-btn">Create Agent</span>
              </div>
              <span class="mock-link">View project settings</span>
            </div>
          </div>

          ${showAttribution
            ? html`<div class="mock-footer" data-preview-attribution>Powered by Scion</div>`
            : nothing}
        </div>
      </div>
    `;
  }

  override render(): TemplateResult {
    return html`
      <section class="preview-card" aria-labelledby="branding-preview-heading">
        <div class="preview-header">
          <div class="preview-title-group">
            <h2 id="branding-preview-heading" class="preview-title">Live Preview</h2>
            <span class="preview-caption">
              Scoped Light and Dark preview of unsaved changes. Live Hub styling updates only after
              saving.
            </span>
          </div>
          ${this.renderSummaryBadge()}
        </div>

        <div class="preview-grid">${this.renderPane('light')} ${this.renderPane('dark')}</div>
      </section>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-branding-preview': ScionBrandingPreview;
  }
}
