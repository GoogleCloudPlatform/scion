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

import {
  css,
  html,
  nothing,
  type ReactiveController,
  type ReactiveControllerHost,
  type TemplateResult,
} from 'lit';
import {
  BRANDING_UPDATED_EVENT,
  getBranding,
  getBrandMonogram,
  getBrandName,
  getBrandSubtitle,
  isCustomBranding,
  shouldShowAttribution,
} from '../../client/branding.js';

/**
 * Shared styles for light/dark logo switching (driven by --scion-logo-*-display
 * in theme.css), compact rail monogram/favicon marks, and "Powered by Scion"
 * attribution footers.
 */
export const brandMarkStyles = css`
  .brand-logo {
    display: block;
    object-fit: contain;
  }

  .brand-logo-light {
    display: var(--scion-logo-light-display, block);
  }

  .brand-logo-dark {
    display: var(--scion-logo-dark-display, none);
  }

  .brand-favicon {
    width: 1.75rem;
    height: 1.75rem;
    object-fit: contain;
    border-radius: var(--scion-radius-sm, 0.25rem);
    flex-shrink: 0;
  }

  .brand-monogram {
    width: 1.75rem;
    height: 1.75rem;
    border-radius: var(--scion-radius-sm, 0.25rem);
    background: var(--scion-primary, #3b82f6);
    color: var(--scion-primary-text, #ffffff);
    font-size: 0.875rem;
    font-weight: 700;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    flex-shrink: 0;
    line-height: 1;
  }

  .brand-logo-wrap {
    display: flex;
    flex-direction: column;
    min-width: 0;
    overflow: hidden;
  }

  .brand-logo-wrap .brand-logo {
    max-height: 28px;
    max-width: 172px;
  }

  .brand-logo-wrap .brand-subtitle {
    font-size: 0.6875rem;
    color: var(--scion-text-muted, #64748b);
    white-space: nowrap;
    margin-top: 0.125rem;
  }

  .brand-attribution {
    font-size: 0.75rem;
    color: var(--scion-text-muted, #64748b);
    text-align: center;
  }
`;

/**
 * Lit ReactiveController that re-renders the host when Hub branding changes
 * live in the current tab (e.g. after saving server.hub.branding).
 */
export class BrandingController implements ReactiveController {
  private readonly host: ReactiveControllerHost;
  private readonly onUpdate = (): void => {
    this.host.requestUpdate();
  };

  constructor(host: ReactiveControllerHost) {
    this.host = host;
    host.addController(this);
  }

  hostConnected(): void {
    if (typeof window !== 'undefined') {
      window.addEventListener(BRANDING_UPDATED_EVENT, this.onUpdate);
    }
  }

  hostDisconnected(): void {
    if (typeof window !== 'undefined') {
      window.removeEventListener(BRANDING_UPDATED_EVENT, this.onUpdate);
    }
  }
}

export function observeBranding(host: ReactiveControllerHost): void {
  new BrandingController(host);
}

/**
 * Renders light/dark brand logo <img> element(s) when a custom logo is configured.
 */
export function renderBrandLogoImages(className = 'brand-logo'): TemplateResult | typeof nothing {
  const b = getBranding();
  const light = b?.logoLight;
  const dark = b?.logoDark;
  if (!light && !dark) return nothing;
  const alt = getBrandName();
  if (light && dark && dark !== light) {
    return html`
      <img class="${className} brand-logo-light" src=${light} alt=${alt} />
      <img class="${className} brand-logo-dark" src=${dark} alt=${alt} />
    `;
  }
  return html`<img class=${className} src=${light || dark || ''} alt=${alt} />`;
}

/**
 * Renders the compact square brand mark (custom favicon, monogram badge, or
 * stock sprout emoji) for the collapsed 64px sidebar rail and icon slots.
 */
export function renderCompactBrandMark(): TemplateResult {
  const b = getBranding();
  if (b?.faviconUrl) {
    return html`<img class="brand-favicon" src=${b.faviconUrl} alt=${getBrandName()} />`;
  }
  if (isCustomBranding()) {
    return html`<span class="brand-monogram" aria-hidden="true">${getBrandMonogram()}</span>`;
  }
  return html`🌱`;
}

/**
 * Renders the sidebar brand header (<scion-nav> and <scion-profile-nav>),
 * switching cleanly between expanded wordmark/logo and collapsed 64px rail.
 */
export function renderSidebarBrand(opts: {
  collapsed: boolean;
  subtitleFallback: string;
  alwaysShowSubtitle?: boolean;
}): TemplateResult {
  const b = getBranding();
  const name = getBrandName();
  const hasLogo = Boolean(b?.logoLight || b?.logoDark);
  const subtitle = opts.alwaysShowSubtitle
    ? opts.subtitleFallback
    : getBrandSubtitle(opts.subtitleFallback);

  if (opts.collapsed) {
    return html`<div class="logo-icon">${renderCompactBrandMark()}</div>`;
  }

  if (hasLogo) {
    return html`
      <div class="brand-logo-wrap">
        ${renderBrandLogoImages('brand-logo')}
        ${subtitle ? html`<span class="brand-subtitle">${subtitle}</span>` : nothing}
      </div>
    `;
  }

  return html`
    <div class="logo-icon">${renderCompactBrandMark()}</div>
    <div class="logo-text">
      <h1>${name}</h1>
      ${subtitle ? html`<span>${subtitle}</span>` : nothing}
    </div>
  `;
}

/**
 * Renders subtle "Powered by Scion" attribution when custom branding is active
 * and attribution is not hidden.
 */
export function renderAttribution(
  className = 'brand-attribution'
): TemplateResult | typeof nothing {
  if (!shouldShowAttribution()) return nothing;
  return html`<div class=${className}>Powered by Scion</div>`;
}
