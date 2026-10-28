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

import { css, html, LitElement, nothing } from 'lit';
import { customElement } from 'lit/decorators.js';
import { live } from 'lit/directives/live.js';
import { LocaleController } from '../../i18n/controller.js';
import { locales } from '../../i18n/locales.js';
import {
  getLocalePreference,
  isLocalizationEnabled,
  setLocalePreference,
  t,
} from '../../i18n/index.js';

@customElement('scion-language-select')
export class ScionLanguageSelect extends LitElement {
  readonly locale = new LocaleController(this);

  static override styles = css`
    :host {
      display: inline-flex;
      min-width: 0;
      margin-inline-start: 0.5rem;
    }
    select {
      font: inherit;
      font-size: 0.875rem;
      max-width: 8rem;
      min-height: 2.75rem;
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius);
      background: var(--scion-surface);
      color: var(--scion-text);
    }
    @media (max-width: 768px) {
      select {
        max-width: 5.5rem;
      }
    }
  `;

  override render() {
    if (!isLocalizationEnabled()) return nothing;
    const preference = getLocalePreference();
    return html`
      <select
        .value=${live(preference)}
        aria-label=${t('language.label')}
        title=${t('language.label')}
        @change=${(event: Event): void => {
          void setLocalePreference((event.target as HTMLSelectElement).value);
        }}
      >
        <option value="" ?selected=${!preference}>${t('language.auto')}</option>
        ${locales.map(
          (locale) =>
            html`<option
              value=${locale.code}
              lang=${locale.code}
              ?selected=${preference === locale.code}
            >
              ${locale.name}
            </option>`
        )}
      </select>
    `;
  }
}
