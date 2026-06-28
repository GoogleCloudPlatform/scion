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
import '../shared/dir-browser.js';

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
  imageRegistry?: string;
  buildAvailable?: boolean;
  gitVersion?: string;
  gitVersionOK?: boolean;
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

  // Step 2b: Apple DNS warning (non-blocking)
  @state() private dnsWarning: string | null = null;

  // Step 3: Harnesses
  @state() private selectedHarnesses = new Set<string>();

  // Step 4: Images
  @state() private imageStatuses = new Map<string, { status: string; error?: string; fullName?: string }>();
  @state() private imagePulling = false;
  @state() private imageBuilding = false;
  @state() private buildLogs: string[] = [];
  @state() private buildExpanded = false;
  @state() private runtimeAvailable = false;
  @state() private buildAvailable = false;
  @state() private imageRegistry = '';
  @state() private gitVersion = '';
  @state() private gitVersionOK = true;
  private imageEventSource: EventSource | null = null;

  // Step 5: Workspace
  @state() private workspaceMode: 'choose' | 'hub' | 'linked' = 'choose';
  @state() private wsProjectName = '';
  @state() private wsLocalPath = '';
  @state() private wsPathValidation: { resolved: string; exists: boolean; isDir: boolean; isGit: boolean; isManaged: boolean; alreadyLinked: boolean; error?: string } | null = null;
  @state() private wsValidatingPath = false;
  @state() private wsCreating = false;
  @state() private wsEmbeddedBrokerID = '';

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

    .warning-banner {
      background: var(--sl-color-warning-50, #fefce8);
      color: var(--sl-color-warning-700, #a16207);
      border: 1px solid var(--sl-color-warning-200, #fef08a);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 0.75rem 1rem;
      margin-bottom: 1rem;
      font-size: 0.875rem;
      white-space: pre-line;
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

    .image-list {
      display: flex;
      flex-direction: column;
      gap: 0.5rem;
      margin-bottom: 1.25rem;
    }

    .image-item {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      padding: 0.625rem 1rem;
      border-radius: var(--scion-radius, 0.5rem);
      border: 1px solid var(--scion-border, #e2e8f0);
      font-size: 0.875rem;
    }

    .image-item .image-name {
      flex: 1;
      font-family: monospace;
      color: var(--scion-text, #1e293b);
    }

    .image-status {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      font-size: 0.75rem;
      font-weight: 600;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      text-transform: uppercase;
      letter-spacing: 0.025em;
    }

    .image-status.queued {
      background: var(--sl-color-neutral-100, #f1f5f9);
      color: var(--sl-color-neutral-600, #475569);
    }

    .image-status.pulling {
      background: var(--sl-color-primary-100, #dbeafe);
      color: var(--sl-color-primary-700, #1d4ed8);
    }

    .image-status.done,
    .image-status.exists {
      background: var(--sl-color-success-100, #dcfce7);
      color: var(--sl-color-success-700, #15803d);
    }

    .image-status.error {
      background: var(--sl-color-danger-100, #fee2e2);
      color: var(--sl-color-danger-700, #b91c1c);
    }

    .image-status sl-spinner {
      font-size: 0.75rem;
    }

    .build-section {
      margin-top: 1.25rem;
      border-top: 1px solid var(--scion-border, #e2e8f0);
      padding-top: 1rem;
    }

    .build-log-toggle {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      cursor: pointer;
      font-size: 0.8rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.75rem;
    }

    .build-log {
      margin-top: 0.5rem;
      max-height: 16rem;
      overflow-y: auto;
      background: var(--sl-color-neutral-50, #f8fafc);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 0.75rem;
      font-family: monospace;
      font-size: 0.75rem;
      line-height: 1.6;
      white-space: pre-wrap;
      word-break: break-all;
    }

    .image-actions {
      display: flex;
      gap: 0.5rem;
      margin-bottom: 1rem;
    }

    .ws-cards {
      display: flex;
      flex-direction: column;
      gap: 0.75rem;
      margin-bottom: 1.25rem;
    }

    .ws-card {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      padding: 1rem;
      border-radius: var(--scion-radius, 0.5rem);
      border: 1px solid var(--scion-border, #e2e8f0);
      cursor: pointer;
      transition: border-color 0.15s;
    }

    .ws-card:hover {
      border-color: var(--scion-primary, #3b82f6);
    }

    .ws-card sl-icon {
      font-size: 1.5rem;
      color: var(--scion-primary, #3b82f6);
      flex-shrink: 0;
    }

    .ws-card .ws-card-text {
      flex: 1;
    }

    .ws-card .ws-card-title {
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      font-size: 0.9375rem;
    }

    .ws-card .ws-card-desc {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.125rem;
    }

    .ws-validation {
      font-size: 0.8125rem;
      margin-top: 0.375rem;
      padding: 0.5rem 0.75rem;
      border-radius: var(--scion-radius, 0.5rem);
    }

    .ws-validation.valid {
      background: var(--sl-color-success-50, #f0fdf4);
      border: 1px solid var(--sl-color-success-200, #bbf7d0);
      color: var(--sl-color-success-700, #15803d);
    }

    .ws-validation.warning {
      background: var(--sl-color-warning-50, #fefce8);
      border: 1px solid var(--sl-color-warning-200, #fef08a);
      color: var(--sl-color-warning-700, #a16207);
    }

    .ws-validation.error {
      background: var(--sl-color-danger-50, #fef2f2);
      border: 1px solid var(--sl-color-danger-200, #fecaca);
      color: var(--sl-color-danger-700, #b91c1c);
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    void this.initialize();
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    this.cleanupImageEvents();
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

      if (status?.imageRegistry) this.imageRegistry = status.imageRegistry;
      if (status?.gitVersion !== undefined) this.gitVersion = status.gitVersion;
      if (status?.gitVersionOK !== undefined) this.gitVersionOK = status.gitVersionOK;

      // Resume: advance past completed steps only if user has previously started
      const previouslyStarted = sessionStorage.getItem('onboardingStarted') === 'true';
      if (status && previouslyStarted) {
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
        ${this.dnsWarning ? html`<div class="warning-banner">${this.dnsWarning}</div>` : nothing}

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
      case 4: return this.renderImages();
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
      sessionStorage.setItem('onboardingStarted', 'true');
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
          ${!this.gitVersionOK && this.gitVersion ? html`
            <div class="check-item">
              <span class="pill warn">warn</span>
              <span class="name">Git version</span>
              <span class="message">
                Git 2.47+ is required for agent worktrees. Detected: ${this.gitVersion}.
                Run <code>brew install git</code> to upgrade.
              </span>
            </div>
          ` : nothing}
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
            <sl-option value="container">Apple Container</sl-option>
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
    this.dnsWarning = null;
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

      if (this.selectedRuntime === 'container') {
        try {
          const dnsRes = await apiFetch('/api/v1/system/apple-dns', { method: 'POST' });
          if (dnsRes.ok) {
            const dnsData = await dnsRes.json() as { configured: boolean; hostname: string; ip: string; error?: string };
            if (!dnsData.configured) {
              this.dnsWarning = `Apple Container DNS setup requires sudo. If not prompted, run manually:\n  sudo container system dns create ${dnsData.hostname} --localhost ${dnsData.ip}`;
            }
          }
        } catch {
          // non-fatal — server startup will retry on next launch
        }
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
      void this.loadImagesStep();
    } finally {
      this.stepLoading = false;
    }
  }

  // ── Step 4: Images ──

  private renderImages() {
    const harnesses = [...this.selectedHarnesses];
    if (harnesses.length === 0) {
      return html`
        <h2>Container Images</h2>
        <p>No harnesses selected. You can go back to select harnesses or skip this step.</p>
        <div class="footer">
          <sl-button variant="text" @click=${() => { this.currentStep = 3; }}>Back</sl-button>
          <div class="footer-right">
            <sl-button variant="default" @click=${() => { this.currentStep = 5; }}>Skip for now</sl-button>
          </div>
        </div>
      `;
    }

    // No registry and no local build — show registry setup guidance
    if (!this.imageRegistry && !this.buildAvailable) {
      return html`
        <h2>Container Images</h2>
        <div class="alert alert-warning">
          <strong>No image registry configured.</strong>
          <p>
            An image registry is required to pull container images.
            Run the following to configure one, then restart the server:
          </p>
          <code>scion config set --global image_registry ghcr.io/homebrew-scion</code>
          <p>If you installed via Homebrew, try reinstalling to auto-configure the registry:</p>
          <code>brew reinstall --HEAD homebrew-scion/scion/scion-workstation</code>
        </div>
        <div class="footer">
          <sl-button variant="text" @click=${() => { this.currentStep = 3; }}>Back</sl-button>
          <div class="footer-right">
            <sl-button variant="default" @click=${() => { this.currentStep = 5; }}>Skip for now</sl-button>
          </div>
        </div>
      `;
    }

    const allDone = harnesses.length > 0 && harnesses.every(h => {
      const s = this.imageStatuses.get(h);
      return s && (s.status === 'done' || s.status === 'exists');
    });

    return html`
      <h2>Container Images</h2>
      <p>Pull or build the container images for your selected harnesses.</p>

      ${!this.runtimeAvailable ? html`
        <div class="alert alert-warning">
          <strong>No container runtime detected.</strong>
          <p>
            Install Docker, Podman, or Apple Container to pull or build images. You can skip this
            step and configure a runtime later.
          </p>
        </div>
      ` : nothing}

      <div class="image-list">
        ${harnesses.map(h => {
          const s = this.imageStatuses.get(h);
          const status = s?.status ?? 'pending';
          const prefix = this.imageRegistry ? `${this.imageRegistry}/` : '';
          const displayName = s?.fullName ?? `${prefix}scion-${h}:latest`;
          return html`
            <div class="image-item">
              <span class="image-name">${displayName}</span>
              ${status === 'pending' ? nothing : html`
                <span class="image-status ${status}">
                  ${status === 'pulling' ? html`<sl-spinner></sl-spinner>` : nothing}
                  ${status === 'done' || status === 'exists' ? '✓' : nothing}
                  ${status === 'error' ? '✗' : nothing}
                  ${status}
                </span>
              `}
            </div>
          `;
        })}
      </div>

      <div class="image-actions">
        ${this.imageRegistry ? html`
          <sl-button
            variant="primary"
            size="small"
            ?loading=${this.imagePulling}
            ?disabled=${this.imagePulling || this.imageBuilding}
            @click=${this.handlePullImages}
          >Pull images</sl-button>
        ` : nothing}

        ${this.buildAvailable ? html`
          <sl-button
            variant=${this.imageRegistry ? 'default' : 'primary'}
            size="small"
            ?loading=${this.imageBuilding}
            ?disabled=${this.imagePulling || this.imageBuilding}
            @click=${this.handleBuildImages}
          >Build locally</sl-button>
        ` : nothing}

        ${!this.imageRegistry && !this.buildAvailable ? html`
          <p style="font-size:0.8125rem;color:var(--scion-text-muted,#64748b);margin:0;">
            Pre-built images are available via pull. Local builds require a source checkout.
          </p>
        ` : nothing}

        ${!this.imageRegistry && this.buildAvailable ? html`
          <p style="font-size:0.8125rem;color:var(--scion-text-muted,#64748b);margin:0;">
            To pull pre-built images instead, configure an image registry.
          </p>
        ` : nothing}
      </div>

      ${this.buildLogs.length > 0 ? html`
        <div class="build-section">
          <div class="build-log-toggle" @click=${() => { this.buildExpanded = !this.buildExpanded; }}>
            <sl-icon name=${this.buildExpanded ? 'chevron-down' : 'chevron-right'}></sl-icon>
            Build output (${this.buildLogs.length} lines)
          </div>
          ${this.buildExpanded ? html`
            <div class="build-log">${this.buildLogs.join('\n')}</div>
          ` : nothing}
        </div>
      ` : nothing}

      <div class="footer">
        <sl-button variant="text" @click=${() => { this.currentStep = 3; }}>Back</sl-button>
        <div class="footer-right">
          <sl-button variant="default" @click=${() => { this.cleanupImageEvents(); this.currentStep = 5; }}>
            Skip for now
          </sl-button>
          ${allDone ? html`
            <sl-button variant="primary" @click=${() => { this.cleanupImageEvents(); this.currentStep = 5; }}>
              Next
            </sl-button>
          ` : nothing}
        </div>
      </div>
    `;
  }

  private async handlePullImages(): Promise<void> {
    this.error = null;
    this.imagePulling = true;
    const harnesses = [...this.selectedHarnesses];

    const statuses = new Map(this.imageStatuses);
    for (const h of harnesses) {
      const prefix = this.imageRegistry ? `${this.imageRegistry}/` : '';
      statuses.set(h, { status: 'queued', fullName: `${prefix}scion-${h}:latest` });
    }
    this.imageStatuses = statuses;

    try {
      const res = await apiFetch('/api/v1/system/images/pull', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ harnesses }),
      });
      if (!res.ok) {
        this.error = await extractApiError(res, 'Failed to start image pull');
        this.imagePulling = false;
        return;
      }
      const data = (await res.json()) as { jobId: string };
      this.subscribeToImageJob(data.jobId, 'pull');
    } catch {
      this.error = 'Failed to connect to the server.';
      this.imagePulling = false;
    }
  }

  private async handleBuildImages(): Promise<void> {
    this.error = null;
    this.imageBuilding = true;
    this.buildLogs = [];
    this.buildExpanded = true;

    try {
      const res = await apiFetch('/api/v1/system/images/build', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ harnesses: [...this.selectedHarnesses] }),
      });
      if (!res.ok) {
        this.error = await extractApiError(res, 'Failed to start image build');
        this.imageBuilding = false;
        return;
      }
      const data = (await res.json()) as { jobId: string };
      this.subscribeToImageJob(data.jobId, 'build');
    } catch {
      this.error = 'Failed to connect to the server.';
      this.imageBuilding = false;
    }
  }

  private subscribeToImageJob(jobId: string, mode: 'pull' | 'build'): void {
    this.cleanupImageEvents();

    const url = `/events?sub=${encodeURIComponent('system.images.' + jobId)}`;
    const es = new EventSource(url);
    this.imageEventSource = es;

    const completedImages = new Set<string>();
    const totalImages = this.selectedHarnesses.size;

    es.addEventListener('update', (event: Event) => {
      try {
        const wrapper = JSON.parse((event as MessageEvent).data) as { subject: string; data?: Record<string, unknown> };
        const d = wrapper.data;
        if (!d) return;

        if (d['image']) {
          const fullImageName = d['image'] as string;
          const status = d['status'] as string;
          const error = d['error'] as string | undefined;

          const harness = this.imageNameToHarness(fullImageName);
          if (harness) {
            const next = new Map(this.imageStatuses);
            const entry: { status: string; error?: string; fullName?: string } = { status, fullName: fullImageName };
            if (error) entry.error = error;
            next.set(harness, entry);
            this.imageStatuses = next;
          }

          if (mode === 'pull' && (status === 'done' || status === 'exists' || status === 'error')) {
            completedImages.add(fullImageName);
            if (completedImages.size >= totalImages) {
              this.imagePulling = false;
              this.cleanupImageEvents();
            }
          }
        } else if (d['status'] === 'error') {
          this.error = (d['error'] as string) || 'An error occurred during image operation.';
          if (mode === 'pull') this.imagePulling = false;
          if (mode === 'build') this.imageBuilding = false;
          this.cleanupImageEvents();
        }

        if (mode === 'build' && d['type'] === 'log') {
          const line = d['line'] as string;
          this.buildLogs = [...this.buildLogs, line];

          if (line === 'build complete' || line.startsWith('build failed:')) {
            this.imageBuilding = false;
            this.cleanupImageEvents();
          }
        }
      } catch (err) {
        console.error('[Onboarding] Failed to parse image event:', err);
      }
    });

    es.onerror = () => {
      if (mode === 'pull') this.imagePulling = false;
      if (mode === 'build') this.imageBuilding = false;
      this.cleanupImageEvents();
    };
  }

  private imageNameToHarness(image: string): string | null {
    const harnessNames = ['claude', 'gemini', 'codex', 'opencode'];
    for (const h of harnessNames) {
      if (image.includes(`scion-${h}`)) return h;
    }
    return null;
  }

  private cleanupImageEvents(): void {
    if (this.imageEventSource) {
      this.imageEventSource.close();
      this.imageEventSource = null;
    }
  }

  private async loadImagesStep(): Promise<void> {
    try {
      const res = await apiFetch('/api/v1/system/runtime');
      if (res.ok) {
        const data = (await res.json()) as RuntimeResponse;
        this.runtimeAvailable = data.available;
      }
    } catch { /* ignore */ }
    try {
      const res = await apiFetch('/api/v1/system/status');
      if (res.ok) {
        const data = (await res.json()) as OnboardingStatus;
        this.buildAvailable = data.buildAvailable ?? false;
      }
    } catch { /* ignore */ }
  }

  // ── Step 5: First Workspace ──

  private renderWorkspacePlaceholder() {
    if (this.workspaceMode === 'hub') return this.renderWsHub();
    if (this.workspaceMode === 'linked') return this.renderWsLinked();
    return this.renderWsChoose();
  }

  private renderWsChoose() {
    return html`
      <h2>First Workspace</h2>
      <p>Create your first project to get started.</p>

      <div class="ws-cards">
        <div class="ws-card" @click=${() => { this.workspaceMode = 'hub'; }}>
          <sl-icon name="cloud"></sl-icon>
          <div class="ws-card-text">
            <div class="ws-card-title">Hub-managed project</div>
            <div class="ws-card-desc">A workspace managed by the Hub. No git repository required.</div>
          </div>
        </div>
        <div class="ws-card" @click=${() => { window.location.href = '/projects/new'; }}>
          <sl-icon name="git"></sl-icon>
          <div class="ws-card-text">
            <div class="ws-card-title">Link a git repo</div>
            <div class="ws-card-desc">Connect to an existing git repository for source-controlled workspaces.</div>
          </div>
        </div>
        <div class="ws-card" @click=${() => { this.workspaceMode = 'linked'; void this.loadWsBrokerID(); }}>
          <sl-icon name="folder-symlink"></sl-icon>
          <div class="ws-card-text">
            <div class="ws-card-title">Add local directory</div>
            <div class="ws-card-desc">Link a local directory. It stays where it is and is operated on in place.</div>
          </div>
        </div>
      </div>

      <div class="footer">
        <sl-button variant="text" @click=${() => { this.currentStep = 4; }}>Back</sl-button>
        <div class="footer-right">
          <sl-button variant="default" @click=${() => { this.currentStep = 6; }}>Skip for now</sl-button>
        </div>
      </div>
    `;
  }

  private renderWsHub() {
    return html`
      <h2>Create Hub Workspace</h2>
      <p>Give your project a name.</p>

      <div class="form-group">
        <label>Project Name</label>
        <sl-input
          placeholder="my-project"
          .value=${this.wsProjectName}
          @sl-input=${(e: Event) => { this.wsProjectName = (e.target as HTMLInputElement).value; }}
        ></sl-input>
      </div>

      <div class="footer">
        <sl-button variant="text" @click=${() => { this.workspaceMode = 'choose'; }}>Back</sl-button>
        <div class="footer-right">
          <sl-button variant="default" @click=${() => { this.currentStep = 6; }}>Skip for now</sl-button>
          <sl-button
            variant="primary"
            ?loading=${this.wsCreating}
            ?disabled=${!this.wsProjectName.trim()}
            @click=${this.handleWsHubCreate}
          >Create & Continue</sl-button>
        </div>
      </div>
    `;
  }

  private async handleWsHubCreate(): Promise<void> {
    this.error = null;
    this.wsCreating = true;
    try {
      const res = await apiFetch('/api/v1/projects', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: this.wsProjectName.trim(), visibility: 'private' }),
      });
      if (!res.ok) {
        this.error = await extractApiError(res, 'Failed to create project');
        return;
      }
      this.currentStep = 6;
    } catch {
      this.error = 'Failed to connect to the server.';
    } finally {
      this.wsCreating = false;
    }
  }

  private renderWsLinked() {
    const pathOk = this.wsPathValidation && !this.wsPathValidation.error && this.wsPathValidation.exists && this.wsPathValidation.isDir;

    return html`
      <h2>Add Local Directory</h2>
      <p>Browse to or enter the path of a local directory.</p>

      <div class="form-group">
        <label>Project Name</label>
        <sl-input
          placeholder="my-project"
          .value=${this.wsProjectName}
          @sl-input=${(e: Event) => { this.wsProjectName = (e.target as HTMLInputElement).value; }}
        ></sl-input>
      </div>

      <div class="form-group">
        <label>Directory</label>
        <scion-dir-browser
          @path-selected=${(e: CustomEvent<{ path: string }>) => {
            this.wsLocalPath = e.detail.path;
            void this.wsValidatePath(e.detail.path);
          }}
        ></scion-dir-browser>
      </div>

      ${this.wsLocalPath ? html`
        <div class="form-group">
          <label>Selected Path</label>
          <sl-input readonly .value=${this.wsLocalPath}></sl-input>
        </div>
      ` : nothing}

      ${this.wsValidatingPath
        ? html`<div class="ws-validation valid" style="display:flex;align-items:center;gap:0.5rem;">
            <sl-spinner style="font-size:0.875rem;"></sl-spinner> Validating…
          </div>`
        : this.wsPathValidation
          ? html`
              ${this.wsPathValidation.error
                ? html`<div class="ws-validation error">${this.wsPathValidation.error}</div>`
                : !this.wsPathValidation.exists
                  ? html`<div class="ws-validation error">Path does not exist.</div>`
                  : !this.wsPathValidation.isDir
                    ? html`<div class="ws-validation error">Not a directory.</div>`
                    : html`<div class="ws-validation valid">Path is valid: ${this.wsPathValidation.resolved}</div>
                        ${this.wsPathValidation.isGit ? html`<div class="ws-validation warning" style="margin-top:0.25rem;">This is a git repository.</div>` : nothing}
                        ${this.wsPathValidation.alreadyLinked ? html`<div class="ws-validation warning" style="margin-top:0.25rem;">Already linked to another project.</div>` : nothing}
                      `}
            `
          : nothing}

      <div class="footer">
        <sl-button variant="text" @click=${() => { this.workspaceMode = 'choose'; }}>Back</sl-button>
        <div class="footer-right">
          <sl-button variant="default" @click=${() => { this.currentStep = 6; }}>Skip for now</sl-button>
          <sl-button
            variant="primary"
            ?loading=${this.wsCreating}
            ?disabled=${!pathOk || !this.wsProjectName.trim()}
            @click=${this.handleWsLinkedCreate}
          >Create & Continue</sl-button>
        </div>
      </div>
    `;
  }

  private async loadWsBrokerID(): Promise<void> {
    if (this.wsEmbeddedBrokerID) return;
    try {
      const res = await apiFetch('/api/v1/system/status');
      if (!res.ok) return;
      const data = (await res.json()) as { embeddedBrokerID?: string };
      if (data.embeddedBrokerID) this.wsEmbeddedBrokerID = data.embeddedBrokerID;
    } catch { /* ignore */ }
  }

  private async wsValidatePath(path: string): Promise<void> {
    this.wsValidatingPath = true;
    this.wsPathValidation = null;
    try {
      const res = await apiFetch('/api/v1/system/fs/validate-path', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ path }),
      });
      if (!res.ok) return;
      this.wsPathValidation = (await res.json()) as typeof this.wsPathValidation;
    } catch { /* ignore */ }
    finally { this.wsValidatingPath = false; }
  }

  private async handleWsLinkedCreate(): Promise<void> {
    if (!this.wsEmbeddedBrokerID) {
      this.error = 'No embedded broker available.';
      return;
    }
    this.error = null;
    this.wsCreating = true;
    try {
      const projRes = await apiFetch('/api/v1/projects', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: this.wsProjectName.trim(), visibility: 'private' }),
      });
      if (!projRes.ok) {
        this.error = await extractApiError(projRes, 'Failed to create project');
        return;
      }
      const projData = (await projRes.json()) as { project?: { id: string }; id?: string };
      const projectId = projData.project?.id || projData.id;
      if (!projectId) { this.error = 'No project ID in response'; return; }

      const provRes = await apiFetch(`/api/v1/projects/${projectId}/providers`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ brokerId: this.wsEmbeddedBrokerID, localPath: this.wsPathValidation!.resolved }),
      });
      if (!provRes.ok) {
        this.error = await extractApiError(provRes, 'Project created but failed to link directory. You can retry.');
        return;
      }
      this.currentStep = 6;
    } catch {
      this.error = 'Failed to connect to the server.';
    } finally {
      this.wsCreating = false;
    }
  }

  // ── Step 6: Done ──

  private renderDone() {
    sessionStorage.setItem('onboardingComplete', 'true');
    sessionStorage.removeItem(ONBOARDING_STATUS_KEY);
    sessionStorage.removeItem('onboardingStarted');

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
