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

/** Disposable legacy route adapter. Retained workspaces mount the pane directly. */
import { LitElement, css, html } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import type { PageData } from '../../shared/types.js';
import { TerminalSessionRegistry } from '../../client/terminal-sessions.js';
import type { ScionTerminalPane } from '../terminal/terminal-pane.js';
import '../terminal/terminal-pane.js';

@customElement('scion-page-terminal')
export class ScionPageTerminal extends LitElement {
  @property({ type: Object }) pageData: PageData | null = null;
  @property({ type: String }) agentId = '';
  @state() private error: string | null = null;
  private pane: ScionTerminalPane | null = null;

  static override styles = css`
    :host {
      display: flex;
      flex: 1;
      min-height: 0;
    }
    scion-terminal-pane {
      flex: 1;
      min-width: 0;
    }
  `;

  protected override firstUpdated(): void {
    if (!this.isConnected) return;
    this.pane = this.shadowRoot!.querySelector('scion-terminal-pane');
    try {
      const accountId = this.pageData?.user?.id;
      if (!accountId) throw new Error('Authentication required to access this terminal.');
      const registry = new TerminalSessionRegistry({
        hubUrl: new URL(import.meta.env.BASE_URL, window.location.origin).href,
        accountId,
      });
      // The legacy router supplies a route snapshot rather than agentId.
      // Resolve it here once; the retained pane never consults location.
      const routeAgentId = this.pageData?.path.match(
        /^\/agents\/([^/?#]+)\/terminal(?:[?#]|$)/
      )?.[1];
      this.pane!.open(registry, this.agentId || routeAgentId || '');
    } catch (error) {
      this.error = error instanceof Error ? error.message : 'Failed to load agent';
    }
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    // Only this disposable adapter closes on navigation. A retained root must not.
    this.pane?.dispose('navigation');
  }

  override render() {
    return this.error
      ? html`<p role="alert">${this.error}</p>`
      : html`<scion-terminal-pane></scion-terminal-pane>`;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-terminal': ScionPageTerminal;
  }
}
