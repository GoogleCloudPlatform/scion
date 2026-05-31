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

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';

const ONBOARDING_STATUS_KEY = 'onboardingStatus';
const TOTAL_STEPS = 6;

interface OnboardingStatus {
  initialized: boolean;
  identitySet: boolean;
  runtimeOK: boolean;
  harnessesSeeded: boolean;
  imagesPresent: boolean;
  hasWorkspace: boolean;
  complete: boolean;
}

interface DiagnosticResult {
  name: string;
  status: 'pass' | 'warn' | 'fail';
  message: string;
}

interface SystemCheckResponse {
  results: DiagnosticResult[];
  ready: boolean;
}

interface RuntimeResponse {
  detected: string;
  configured: string;
  available: boolean;
}

@customElement('scion-page-onboarding')
export class ScionPageOnboarding extends LitElement {
  @state() private currentStep = 0;
  @state() private loading = true;
  @state() private stepLoading = false;
  @state() private error: string | null = null;

  // Step 0: Identity
  @state() private displayName = '';
  @state() private email = '';

  // Step 1: System Check
  @state() private checkResults: DiagnosticResult[] = [];
  @state() private checkReady = false;

  // Step 2: Runtime
  @state() private detectedRuntime = '';
  @state() private configuredRuntime = '';
  @state() private selectedRuntime = '';

  // Step 3: Harnesses
  @state() private selectedHarnesses = new Set<string>();

  static override styles = css`
    :host {
      display: flex;
      align-items: center;
      justify-content: center;
      min-height: 100vh;
      background: var(--scion-bg, #f8fafc);
      font-family: var(--scion-font, system-ui, -apple-system, sans-serif);
    }

    .wizard {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 2.5rem;
      max-width: 36rem;
      width: 100%;
      box-shadow: 0 1px 3px rgba(0, 0, 0, 0.1);
    }

    .progress {
      margin-bottom: 2rem;
    }

    .step-label {
      font-size: 0.8rem;
      color: var(--scion-text-muted, #64748b);
      margin-bottom: 0.5rem;
    }

    h1 {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.5rem 0;
    }

    h2 {
      font-size: 1.25rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.25rem 0;
    }

    p {
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 1.5rem 0;
      line-height: 1.5;
    }

    .form-group {
      margin-bottom: 1.25rem;
    }

    .form-group label {
      display: block;
      font-size: 0.875rem;
      font-weight: 500;
      color: var(--scion-text, #1e293b);
      margin-bottom: 0.375rem;
    }

    .footer {
      display: flex;
      justify-content: space-between;
      align-items: center;
      margin-top: 2rem;
      padding-top: 1.5rem;
      border-top: 1px solid var(--scion-border, #e2e8f0);
    }

    .footer-right {
      display: flex;
      gap: 0.5rem;
    }

    .error-banner {
      background: var(--sl-color-danger-50, #fef2f2);
      color: var(--sl-color-danger-700, #b91c1c);
      border: 1px solid var(--sl-color-danger-200, #fecaca);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 0.75rem 1rem;
      margin-bottom: 1rem;
      font-size: 0.875rem;
    }

    .check-results {
      display: flex;
      flex-direction: column;
      gap: 0.75rem;
      margin-bottom: 1rem;
    }

    .check-item {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      padding: 0.75rem 1rem;
      border-radius: var(--scion-radius, 0.5rem);
      border: 1px solid var(--scion-border, #e2e8f0);
    }

    .check-item .name {
      font-weight: 500;
      color: var(--scion-text, #1e293b);
      min-width: 5rem;
    }

    .check-item .message {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
      flex: 1;
    }

    .pill {
      display: inline-block;
      font-size: 0.75rem;
      font-weight: 600;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      text-transform: uppercase;
      letter-spacing: 0.025em;
    }

    .pill.pass {
      background: var(--sl-color-success-100, #dcfce7);
      color: var(--sl-color-success-700, #15803d);
    }

    .pill.warn {
      background: var(--sl-color-warning-100, #fef9c3);
      color: var(--sl-color-warning-700, #a16207);
    }

    .pill.fail {
      background: var(--sl-color-danger-100, #fee2e2);
      color: var(--sl-color-danger-700, #b91c1c);
    }

    .runtime-info {
      padding: 1rem;
      border-radius: var(--scion-radius, 0.5rem);
      border: 1px solid var(--scion-border, #e2e8f0);
      margin-bottom: 1.25rem;
    }

    .runtime-detected {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      margin-bottom: 0.25rem;
    }

    .runtime-detected strong {
      color: var(--scion-text, #1e293b);
    }

    .harness-list {
      display: flex;
      flex-direction: column;
      gap: 0.75rem;
      margin-bottom: 1rem;
    }

    .harness-item {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      padding: 0.75rem 1rem;
      border-radius: var(--scion-radius, 0.5rem);
      border: 1px solid var(--scion-border, #e2e8f0);
    }

    .harness-item .harness-name {
      font-weight: 500;
      color: var(--scion-text, #1e293b);
    }

    .placeholder-content {
      text-align: center;
      padding: 2rem 1rem;
    }

    .placeholder-content sl-icon {
      font-size: 2.5rem;
      color: var(--scion-text-muted, #64748b);
      margin-bottom: 1rem;
    }

    .done-content {
      text-align: center;
      padding: 1rem 0;
    }

    .done-content sl-icon {
      font-size: 3rem;
      color: var(--sl-color-success-500, #22c55e);
      margin-bottom: 1rem;
    }

    .loading-state {
      display: flex;
      flex-direction: column;
      align-items: center;
      gap: 1rem;
      padding: 2rem 0;
    }

    .loading-state sl-spinner {
      font-size: 2rem;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    void this.initialize();
  }

  private async initialize(): Promise<void> {
    try {
      const stored = sessionStorage.getItem(ONBOARDING_STATUS_KEY);
      let status: OnboardingStatus | null = null;

      if (stored) {
        try {
          status = JSON.parse(stored) as OnboardingStatus;
        } catch { /* ignore parse errors */ }
      }

      if (!status) {
        const res = await apiFetch('/api/v1/system/status');
        if (res.ok) {
          status = (await res.json()) as OnboardingStatus;
          sessionStorage.setItem(ONBOARDING_STATUS_KEY, JSON.stringify(status));
        }
      }

      // Resume: advance past completed steps
      if (status) {
        if (status.identitySet && this.currentStep === 0) this.currentStep = 1;
        if (status.runtimeOK && this.currentStep <= 2) this.currentStep = Math.max(this.currentStep, 3);
        if (status.harnessesSeeded && this.currentStep <= 3) this.currentStep = Math.max(this.currentStep, 4);
      }

      // Prefill identity from current user
      try {
        const meRes = await apiFetch('/api/v1/auth/me');
        if (meRes.ok) {
          const me = (await meRes.json()) as { displayName?: string; email?: string };
          if (me.displayName) this.displayName = me.displayName;
          if (me.email) this.email = me.email;
        }
      } catch { /* ignore */ }
    } finally {
      this.loading = false;
    }
  }

  override render() {
    if (this.loading) {
      return html`
        <div class="wizard">
          <div class="loading-state">
            <sl-spinner></sl-spinner>
            <p>Loading...</p>
          </div>
        </div>
      `;
    }

    return html`
      <div class="wizard">
        ${this.currentStep < TOTAL_STEPS ? html`
          <div class="progress">
            <div class="step-label">Step ${this.currentStep + 1} of ${TOTAL_STEPS}</div>
            <sl-progress-bar value=${Math.round((this.currentStep / TOTAL_STEPS) * 100)}></sl-progress-bar>
          </div>
        ` : nothing}

        ${this.error ? html`<div class="error-banner">${this.error}</div>` : nothing}

        ${this.renderStep()}
      </div>
    `;
  }

  private renderStep() {
    switch (this.currentStep) {
      case 0: return this.renderIdentity();
      case 1: return this.renderSystemCheck();
      case 2: return this.renderRuntime();
      case 3: return this.renderHarnesses();
      case 4: return this.renderImagesPlaceholder();
      case 5: return this.renderWorkspacePlaceholder();
      case 6: return this.renderDone();
      default: return nothing;
    }
  }

  // ── Step 0: Welcome / Identity ──

  private renderIdentity() {
    return html`
      <h1>Welcome to Scion</h1>
      <p>Let's get your workstation set up. First, tell us who you are.</p>

      <div class="form-group">
        <label>Display Name</label>
        <sl-input
          placeholder="Your name"
          value=${this.displayName}
          @sl-input=${(e: Event) => { this.displayName = (e.target as HTMLInputElement).value; }}
        ></sl-input>
      </div>

      <div class="form-group">
        <label>Email</label>
        <sl-input
          type="email"
          placeholder="you@example.com"
          value=${this.email}
          @sl-input=${(e: Event) => { this.email = (e.target as HTMLInputElement).value; }}
        ></sl-input>
      </div>

      <div class="footer">
        <div></div>
        <div class="footer-right">
          <sl-button
            variant="primary"
            ?loading=${this.stepLoading}
            @click=${this.handleIdentityNext}
          >Next</sl-button>
        </div>
      </div>
    `;
  }

  private async handleIdentityNext(): Promise<void> {
    if (!this.displayName.trim() && !this.email.trim()) {
      this.error = 'Please enter at least a display name or email.';
      return;
    }

    this.error = null;
    this.stepLoading = true;
    try {
      const res = await apiFetch('/api/v1/system/identity', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ displayName: this.displayName.trim(), email: this.email.trim() }),
      });
      if (!res.ok) {
        this.error = await extractApiError(res, 'Failed to save identity');
        return;
      }
      this.currentStep = 1;
      void this.loadSystemCheck();
    } finally {
      this.stepLoading = false;
    }
  }

  // ── Step 1: System Check ──

  private renderSystemCheck() {
    return html`
      <h2>System Check</h2>
      <p>Verifying your environment is ready.</p>

      ${this.stepLoading ? html`
        <div class="loading-state">
          <sl-spinner></sl-spinner>
          <p>Running checks...</p>
        </div>
      ` : html`
        <div class="check-results">
          ${this.checkResults.map(r => html`
            <div class="check-item">
              <span class="pill ${r.status}">${r.status}</span>
              <span class="name">${r.name}</span>
              <span class="message">${r.message}</span>
            </div>
          `)}
        </div>
      `}

      <div class="footer">
        <sl-button variant="text" @click=${() => { this.currentStep = 0; }}>Back</sl-button>
        <div class="footer-right">
          <sl-button variant="default" ?loading=${this.stepLoading} @click=${() => { void this.loadSystemCheck(); }}>
            Re-check
          </sl-button>
          <sl-button
            variant="primary"
            ?disabled=${!this.checkReady || this.stepLoading}
            @click=${() => { this.currentStep = 2; void this.loadRuntime(); }}
          >Next</sl-button>
        </div>
      </div>
    `;
  }

  private async loadSystemCheck(): Promise<void> {
    this.stepLoading = true;
    this.error = null;
    try {
      const res = await apiFetch('/api/v1/system/check');
      if (!res.ok) {
        this.error = await extractApiError(res, 'System check failed');
        return;
      }
      const data = (await res.json()) as SystemCheckResponse;
      this.checkResults = data.results;
      this.checkReady = data.ready;
    } catch {
      this.error = 'Failed to connect to the server.';
    } finally {
      this.stepLoading = false;
    }
  }

  // ── Step 2: Runtime ──

  private renderRuntime() {
    return html`
      <h2>Container Runtime</h2>
      <p>Select the container runtime for your workstation.</p>

      ${this.stepLoading ? html`
        <div class="loading-state">
          <sl-spinner></sl-spinner>
          <p>Detecting runtime...</p>
        </div>
      ` : html`
        <div class="runtime-info">
          <div class="runtime-detected">
            Detected: <strong>${this.detectedRuntime || 'none'}</strong>
          </div>
          ${this.configuredRuntime ? html`
            <div class="runtime-detected">
              Currently configured: <strong>${this.configuredRuntime}</strong>
            </div>
          ` : nothing}
        </div>

        <div class="form-group">
          <label>Runtime</label>
          <sl-select
            value=${this.selectedRuntime}
            @sl-change=${(e: Event) => { this.selectedRuntime = (e.target as HTMLSelectElement).value; }}
          >
            <sl-option value="docker">Docker</sl-option>
            <sl-option value="podman">Podman</sl-option>
            <sl-option value="container">Container (generic)</sl-option>
          </sl-select>
        </div>
      `}

      <div class="footer">
        <sl-button variant="text" @click=${() => { this.currentStep = 1; }}>Back</sl-button>
        <div class="footer-right">
          <sl-button
            variant="primary"
            ?loading=${this.stepLoading}
            ?disabled=${!this.selectedRuntime}
            @click=${this.handleRuntimeNext}
          >Next</sl-button>
        </div>
      </div>
    `;
  }

  private async loadRuntime(): Promise<void> {
    this.stepLoading = true;
    this.error = null;
    try {
      const res = await apiFetch('/api/v1/system/runtime');
      if (!res.ok) {
        this.error = await extractApiError(res, 'Failed to load runtime info');
        return;
      }
      const data = (await res.json()) as RuntimeResponse;
      this.detectedRuntime = data.detected;
      this.configuredRuntime = data.configured;
      this.selectedRuntime = data.configured || data.detected || 'docker';
    } catch {
      this.error = 'Failed to connect to the server.';
    } finally {
      this.stepLoading = false;
    }
  }

  private async handleRuntimeNext(): Promise<void> {
    this.error = null;
    this.stepLoading = true;
    try {
      const res = await apiFetch('/api/v1/system/runtime', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ runtime: this.selectedRuntime }),
      });
      if (!res.ok) {
        this.error = await extractApiError(res, 'Failed to save runtime');
        return;
      }
      this.currentStep = 3;
    } finally {
      this.stepLoading = false;
    }
  }

  // ── Step 3: Harnesses ──

  private renderHarnesses() {
    const harnesses = [
      { id: 'claude', label: 'Claude Code' },
      { id: 'gemini', label: 'Gemini' },
      { id: 'codex', label: 'Codex' },
      { id: 'opencode', label: 'OpenCode' },
    ];

    return html`
      <h2>AI Harnesses</h2>
      <p>Select which AI coding harnesses to configure.</p>

      <div class="harness-list">
        ${harnesses.map(h => html`
          <div class="harness-item">
            <sl-checkbox
              ?checked=${this.selectedHarnesses.has(h.id)}
              @sl-change=${(e: Event) => {
                const checked = (e.target as HTMLInputElement).checked;
                const next = new Set(this.selectedHarnesses);
                if (checked) { next.add(h.id); } else { next.delete(h.id); }
                this.selectedHarnesses = next;
              }}
            >${h.label}</sl-checkbox>
          </div>
        `)}
      </div>

      <div class="footer">
        <sl-button variant="text" @click=${() => { this.currentStep = 2; }}>Back</sl-button>
        <div class="footer-right">
          <sl-button
            variant="primary"
            ?loading=${this.stepLoading}
            ?disabled=${this.selectedHarnesses.size === 0}
            @click=${this.handleHarnessesNext}
          >Next</sl-button>
        </div>
      </div>
    `;
  }

  private async handleHarnessesNext(): Promise<void> {
    this.error = null;
    this.stepLoading = true;
    try {
      const res = await apiFetch('/api/v1/system/init', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ harnesses: [...this.selectedHarnesses] }),
      });
      if (!res.ok) {
        this.error = await extractApiError(res, 'Failed to initialize harnesses');
        return;
      }
      this.currentStep = 4;
    } finally {
      this.stepLoading = false;
    }
  }

  // ── Step 4: Images (placeholder) ──

  private renderImagesPlaceholder() {
    return html`
      <h2>Container Images</h2>
      <div class="placeholder-content">
        <sl-icon name="box-seam"></sl-icon>
        <p>Image management will be available in a future update.</p>
      </div>

      <div class="footer">
        <sl-button variant="text" @click=${() => { this.currentStep = 3; }}>Back</sl-button>
        <div class="footer-right">
          <sl-button variant="default" @click=${() => { this.currentStep = 5; }}>Skip for now</sl-button>
        </div>
      </div>
    `;
  }

  // ── Step 5: First Workspace (placeholder) ──

  private renderWorkspacePlaceholder() {
    return html`
      <h2>First Workspace</h2>
      <div class="placeholder-content">
        <sl-icon name="folder-plus"></sl-icon>
        <p>Workspace creation will be available in a future update.</p>
      </div>

      <div class="footer">
        <sl-button variant="text" @click=${() => { this.currentStep = 4; }}>Back</sl-button>
        <div class="footer-right">
          <sl-button variant="default" @click=${() => { this.currentStep = 6; }}>Skip for now</sl-button>
        </div>
      </div>
    `;
  }

  // ── Step 6: Done ──

  private renderDone() {
    sessionStorage.setItem('onboardingComplete', 'true');
    sessionStorage.removeItem(ONBOARDING_STATUS_KEY);

    return html`
      <div class="done-content">
        <sl-icon name="check-circle"></sl-icon>
        <h1>You're All Set</h1>
        <p>Your workstation is configured and ready to use.</p>
        <sl-button variant="primary" size="large" @click=${() => { window.location.href = '/'; }}>
          Go to Dashboard
        </sl-button>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-onboarding': ScionPageOnboarding;
  }
}
