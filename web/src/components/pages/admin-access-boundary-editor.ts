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
 * Admin Access Boundary Editor page component.
 *
 * Full-page 6-step guided authoring workflow for creating and editing
 * access boundaries:
 *
 *   1. Name and Purpose
 *   2. Subject Selection
 *   3. Scope Selection
 *   4. Maximum Permissions
 *   5. Activation Window (optional)
 *   6. Review Summary
 *
 * Handles both create and edit modes based on URL.
 * Maintains local draft state across step navigation.
 * Dirty-draft detection warns on browser navigation if draft has unsaved changes.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import { setDocumentTitle } from '../../client/page-title.js';
import { navigateTo } from '../../client/main.js';
import * as accessBoundariesApi from '../../client/access-boundaries-api.js';
import type {
  ConstraintSubject,
  ConstraintScope,
  PermissionId,
  Iso8601,
  SubjectSelection,
  AccessBoundaryDetail,
  AccessConstraintDraft,
  PreviewOperation,
} from '../../shared/access-boundaries.js';
import { subjectSelectionOf } from '../../shared/access-boundaries.js';

// Import sub-components
import '../shared/access-boundary-stepper.js';
import '../shared/access-boundary-subject-selector.js';
import '../shared/access-boundary-scope-selector.js';
import '../shared/maximum-permission-selector.js';
import '../shared/access-boundary-schedule-editor.js';
import '../shared/access-boundary-definition-summary.js';
import '../shared/access-boundary-preview.js';

import type { SubjectChangeDetail } from '../shared/access-boundary-subject-selector.js';
import type { ScopeChangeDetail } from '../shared/access-boundary-scope-selector.js';
import type { PermissionChangeDetail } from '../shared/maximum-permission-selector.js';
import type { ScheduleChangeDetail } from '../shared/access-boundary-schedule-editor.js';
import type { DefinitionSummaryData } from '../shared/access-boundary-definition-summary.js';
import type { PreviewCommitSuccessDetail } from '../shared/access-boundary-preview.js';

@customElement('scion-page-admin-access-boundary-editor')
export class ScionPageAdminAccessBoundaryEditor extends LitElement {
  // --- Mode ---
  @state() private isEditMode = false;
  @state() private boundaryId = '';
  @state() private loadingBoundary = false;
  @state() private loadError = '';

  // --- Stepper ---
  @state() private currentStep = 1;
  @state() private completedSteps: number[] = [];

  // --- Draft state ---
  @state() private draftName = '';
  @state() private draftPurpose = '';
  @state() private draftSubject: ConstraintSubject | null = null;
  @state() private draftSubjectSelection: SubjectSelection = 'exact_user';
  @state() private draftSubjectLabel = '';
  @state() private draftScope: ConstraintScope | null = null;
  @state() private draftScopeType: 'system' | 'project' = 'system';
  @state() private draftProjectId = '';
  @state() private draftProjectLabel = '';
  @state() private draftRetainedPermissions: PermissionId[] = [];
  @state() private draftTotalPermissionCount = 0;
  @state() private draftNewSincePermissionIds: PermissionId[] = [];
  @state() private draftNotBefore: Iso8601 | undefined = undefined;
  @state() private draftExpiresAt: Iso8601 | undefined = undefined;

  // --- Edit mode metadata ---
  /**
   * Base revision for optimistic concurrency in edit mode.
   * Used by F4's commit flow; stored here so it survives step navigation.
   */
  @state() accessor baseRevision = '';
  @state() private isDirty = false;

  // --- Preview/commit flow ---
  @state() private showPreview = false;

  // --- Validation ---
  @state() private step1Error = '';

  // Bound event handlers for dirty-draft detection
  private boundBeforeUnload = this.handleBeforeUnload.bind(this);
  private boundPopState = this.handlePopState.bind(this);

  static override styles = css`
    :host {
      display: block;
    }

    .editor-page {
      max-width: 56rem;
      margin: 0 auto;
      padding: 1rem 1.5rem 3rem;
    }

    .editor-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-bottom: 0.5rem;
    }

    .editor-title {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }

    .editor-back-link {
      font-size: 0.875rem;
      color: var(--sl-color-primary-600, #2563eb);
      text-decoration: none;
      display: flex;
      align-items: center;
      gap: 0.25rem;
      cursor: pointer;
    }

    .editor-back-link:hover {
      text-decoration: underline;
    }

    .step-content {
      padding: 1.5rem 0;
    }

    .step-title {
      font-size: 1.125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.25rem 0;
    }

    .step-description {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 1.25rem 0;
    }

    .step-body {
      margin-bottom: 1.5rem;
    }

    .step-navigation {
      display: flex;
      justify-content: space-between;
      align-items: center;
      padding-top: 1rem;
      border-top: 1px solid var(--scion-border, #e2e8f0);
    }

    .nav-left,
    .nav-right {
      display: flex;
      gap: 0.5rem;
    }

    .loading-state,
    .error-state {
      text-align: center;
      padding: 4rem 2rem;
      color: var(--scion-text-muted, #64748b);
    }

    .error-state {
      color: var(--sl-color-danger-600, #dc2626);
    }

    .form-field {
      margin-bottom: 1rem;
    }

    .char-count {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      text-align: right;
      margin-top: 0.25rem;
    }

    .field-error {
      font-size: 0.75rem;
      color: var(--sl-color-danger-600, #dc2626);
      margin-top: 0.25rem;
    }

    /* Responsive: mobile */
    @media (max-width: 768px) {
      .editor-page {
        padding: 0.5rem 0.75rem 2rem;
      }

      .editor-title {
        font-size: 1.25rem;
      }

      .editor-header {
        flex-direction: column;
        gap: 0.5rem;
      }

      .step-navigation {
        flex-direction: column;
        gap: 0.75rem;
      }

      .nav-left,
      .nav-right {
        width: 100%;
        justify-content: stretch;
      }

      .nav-left sl-button,
      .nav-right sl-button {
        flex: 1;
      }
    }

    /* Forced colors */
    @media (forced-colors: active) {
      .field-error {
        color: LinkText;
        font-weight: bold;
      }
    }

    /* Reduced motion */
    @media (prefers-reduced-motion: reduce) {
      * {
        transition: none !important;
      }
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    const match = window.location.pathname.match(/\/admin\/access-boundaries\/([^/]+)\/edit/);
    if (match) {
      this.isEditMode = true;
      this.boundaryId = match[1];
      setDocumentTitle('Edit Access Boundary');
      void this.loadBoundary();
    } else {
      this.isEditMode = false;
      setDocumentTitle('New Access Boundary');
      // Default scope to system for new boundaries
      this.draftScope = { type: 'system' };
      this.draftScopeType = 'system';
    }
    window.addEventListener('beforeunload', this.boundBeforeUnload);
    window.addEventListener('popstate', this.boundPopState);
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    window.removeEventListener('beforeunload', this.boundBeforeUnload);
    window.removeEventListener('popstate', this.boundPopState);
  }

  private handleBeforeUnload(e: BeforeUnloadEvent): void {
    if (this.isDirty) {
      e.preventDefault();
    }
  }

  private handlePopState(): void {
    if (this.isDirty) {
      const leave = confirm('You have unsaved changes. Are you sure you want to leave?');
      if (!leave) {
        // Push current state back to prevent navigation
        window.history.pushState(
          {},
          '',
          this.isEditMode
            ? `/admin/access-boundaries/${encodeURIComponent(this.boundaryId)}/edit`
            : '/admin/access-boundaries/new'
        );
      }
    }
  }

  private async loadBoundary(): Promise<void> {
    this.loadingBoundary = true;
    this.loadError = '';
    try {
      const boundary: AccessBoundaryDetail = await accessBoundariesApi.get(this.boundaryId);
      this.populateFromBoundary(boundary);
    } catch (err) {
      this.loadError = err instanceof Error ? err.message : 'Failed to load access boundary';
      console.error('Failed to load access boundary:', err);
    } finally {
      this.loadingBoundary = false;
    }
  }

  private populateFromBoundary(boundary: AccessBoundaryDetail): void {
    this.draftName = boundary.name ?? '';
    this.draftPurpose = boundary.purpose ?? '';
    this.baseRevision = boundary.revision ?? '';

    // Subject
    if (boundary.subject) {
      this.draftSubject = boundary.subject;
      this.draftSubjectSelection = subjectSelectionOf(boundary.subject);
      this.draftSubjectLabel = boundary.subjectDisplay?.label ?? '';
    }

    // Scope
    if (boundary.scope) {
      this.draftScope = boundary.scope;
      this.draftScopeType = boundary.scope.type;
      if (boundary.scope.type === 'project') {
        this.draftProjectId = boundary.scope.projectId;
        this.draftProjectLabel = boundary.scopeDisplay?.projectName ?? boundary.scope.projectId;
      }
    }

    // Permissions
    if (boundary.maximumPermissions) {
      this.draftRetainedPermissions = [...boundary.maximumPermissions];
    }
    if (boundary.permissionRegistry) {
      this.draftTotalPermissionCount = boundary.permissionRegistry.totalPermissionCount ?? 0;
      this.draftNewSincePermissionIds = [
        ...(boundary.permissionRegistry.newSincePermissionIds ?? []),
      ];
    }

    // Schedule
    if (boundary.appliesWhen) {
      this.draftNotBefore = boundary.appliesWhen.notBefore ?? undefined;
      this.draftExpiresAt = boundary.appliesWhen.expiresAt ?? undefined;
    }

    // Mark all steps completed since we're editing an existing boundary
    this.completedSteps = [1, 2, 3, 4, 5];
  }

  // ---------------------------------------------------------------------------
  // Step navigation
  // ---------------------------------------------------------------------------

  private navigateToStep(step: number): void {
    if (step >= 1 && step <= 6) {
      this.currentStep = step;
    }
  }

  private handleStepNavigate(e: CustomEvent<number>): void {
    this.navigateToStep(e.detail);
  }

  private goNext(): void {
    // Validate current step before advancing
    if (!this.validateCurrentStep()) return;

    // Mark current step as completed
    if (!this.completedSteps.includes(this.currentStep)) {
      this.completedSteps = [...this.completedSteps, this.currentStep];
    }
    this.navigateToStep(this.currentStep + 1);
  }

  private goBack(): void {
    this.navigateToStep(this.currentStep - 1);
  }

  private validateCurrentStep(): boolean {
    this.step1Error = '';

    if (this.currentStep === 1) {
      if (!this.draftName.trim()) {
        this.step1Error = 'Name is required.';
        return false;
      }
      if (!this.draftPurpose.trim()) {
        this.step1Error = 'Purpose is required.';
        return false;
      }
      if (this.draftPurpose.trim().toLowerCase() === this.draftName.trim().toLowerCase()) {
        this.step1Error =
          'Purpose must explain the security intent — it cannot be the same as the name.';
        return false;
      }
    }

    return true;
  }

  private markDirty(): void {
    this.isDirty = true;
  }

  // ---------------------------------------------------------------------------
  // Event handlers for sub-components
  // ---------------------------------------------------------------------------

  private handleNameInput(e: Event): void {
    this.draftName = (e.target as HTMLInputElement).value;
    this.markDirty();
  }

  private handlePurposeInput(e: Event): void {
    this.draftPurpose = (e.target as HTMLTextAreaElement).value;
    this.markDirty();
  }

  private handleSubjectChange(e: CustomEvent<SubjectChangeDetail>): void {
    this.draftSubject = e.detail.subject;
    this.draftSubjectSelection = e.detail.selection;
    this.draftSubjectLabel = e.detail.displayLabel;
    this.markDirty();
  }

  private handleScopeChange(e: CustomEvent<ScopeChangeDetail>): void {
    this.draftScope = e.detail.scope;
    if (e.detail.scope) {
      this.draftScopeType = e.detail.scope.type;
      if (e.detail.scope.type === 'project') {
        this.draftProjectId = e.detail.scope.projectId;
      } else {
        this.draftProjectId = '';
      }
    }
    this.draftProjectLabel = e.detail.displayLabel;
    this.markDirty();
  }

  private handlePermissionChange(e: CustomEvent<PermissionChangeDetail>): void {
    this.draftRetainedPermissions = e.detail.retainedPermissions;
    this.draftTotalPermissionCount = e.detail.totalCount;
    this.markDirty();
  }

  private handleScheduleChange(e: CustomEvent<ScheduleChangeDetail>): void {
    this.draftNotBefore = e.detail.notBefore;
    this.draftExpiresAt = e.detail.expiresAt;
    this.markDirty();
  }

  private handleSummaryNavigate(e: CustomEvent<number>): void {
    this.navigateToStep(e.detail);
  }

  // ---------------------------------------------------------------------------
  // Draft building and preview/commit flow
  // ---------------------------------------------------------------------------

  private buildDraft(): AccessConstraintDraft | null {
    if (!this.draftSubject || !this.draftScope) return null;

    const draft: AccessConstraintDraft = {
      name: this.draftName.trim(),
      purpose: this.draftPurpose.trim(),
      subject: this.draftSubject,
      scope: this.draftScope,
      maximumPermissions: [...this.draftRetainedPermissions],
    };

    if (this.draftNotBefore || this.draftExpiresAt) {
      draft.appliesWhen = {};
      if (this.draftNotBefore) draft.appliesWhen.notBefore = this.draftNotBefore;
      if (this.draftExpiresAt) draft.appliesWhen.expiresAt = this.draftExpiresAt;
    }

    return draft;
  }

  private get previewOperation(): PreviewOperation {
    return this.isEditMode ? 'update' : 'create';
  }

  private handleStartPreview(): void {
    this.showPreview = true;
  }

  private handlePreviewCommitSuccess(e: CustomEvent<PreviewCommitSuccessDetail>): void {
    this.isDirty = false;
    navigateTo(`/admin/access-boundaries/${encodeURIComponent(e.detail.boundaryId)}`);
  }

  private handlePreviewCancel(): void {
    this.showPreview = false;
  }

  // ---------------------------------------------------------------------------
  // Render
  // ---------------------------------------------------------------------------

  override render() {
    if (this.loadingBoundary) {
      return html`
        <div class="loading-state">
          <sl-spinner style="font-size: 2rem"></sl-spinner>
          <p>Loading access boundary...</p>
        </div>
      `;
    }

    if (this.loadError) {
      return html`
        <div class="error-state">
          <sl-icon name="exclamation-circle" style="font-size: 2rem"></sl-icon>
          <p>${this.loadError}</p>
          <sl-button
            variant="default"
            @click=${() => {
              window.location.href = '/admin/access-boundaries';
            }}
          >
            Back to inventory
          </sl-button>
        </div>
      `;
    }

    return html`
      <div class="editor-page">
        <div class="editor-header">
          <h1 class="editor-title">
            ${this.isEditMode ? 'Edit Access Boundary' : 'New Access Boundary'}
          </h1>
          <a
            class="editor-back-link"
            href="/admin/access-boundaries"
            @click=${(e: Event) => {
              if (this.isDirty) {
                if (!confirm('You have unsaved changes. Are you sure you want to leave?')) {
                  e.preventDefault();
                  return;
                }
              }
            }}
          >
            <sl-icon name="arrow-left"></sl-icon>
            Back to inventory
          </a>
        </div>

        <scion-access-boundary-stepper
          .currentStep=${this.currentStep}
          .completedSteps=${this.completedSteps}
          @step-navigate=${(e: CustomEvent<number>) => this.handleStepNavigate(e)}
        ></scion-access-boundary-stepper>

        <div class="step-content">${this.renderCurrentStep()}</div>
      </div>
    `;
  }

  private renderCurrentStep() {
    switch (this.currentStep) {
      case 1:
        return this.renderStep1();
      case 2:
        return this.renderStep2();
      case 3:
        return this.renderStep3();
      case 4:
        return this.renderStep4();
      case 5:
        return this.renderStep5();
      case 6:
        return this.renderStep6();
      default:
        return nothing;
    }
  }

  // ---------------------------------------------------------------------------
  // Step 1: Name and Purpose
  // ---------------------------------------------------------------------------
  private renderStep1() {
    return html`
      <h2 class="step-title">Name and Purpose</h2>
      <p class="step-description">
        Give this access boundary a descriptive name and explain its security intent.
      </p>

      <div class="step-body">
        <div class="form-field">
          <sl-input
            label="Name"
            placeholder="e.g., Contractor read-only boundary"
            value=${this.draftName}
            required
            @sl-input=${(e: Event) => this.handleNameInput(e)}
            help-text="Must be unique within the selected scope"
          ></sl-input>
          <div class="char-count">${this.draftName.length} characters</div>
        </div>

        <div class="form-field">
          <sl-textarea
            label="Purpose"
            placeholder="Explain the security intent of this access boundary..."
            value=${this.draftPurpose}
            required
            rows="4"
            resize="auto"
            @sl-input=${(e: Event) => this.handlePurposeInput(e)}
            help-text="Describe why this boundary exists and what it protects"
          ></sl-textarea>
          <div class="char-count">${this.draftPurpose.length} characters</div>
        </div>

        ${this.step1Error
          ? html`<div class="field-error" role="alert" aria-live="assertive">
              ${this.step1Error}
            </div>`
          : nothing}
      </div>

      ${this.renderStepNavigation()}
    `;
  }

  // ---------------------------------------------------------------------------
  // Step 2: Subject Selection
  // ---------------------------------------------------------------------------
  private renderStep2() {
    return html`
      <h2 class="step-title">Subject</h2>
      <p class="step-description">Choose who this access boundary applies to.</p>

      <div class="step-body">
        <scion-access-boundary-subject-selector
          .selection=${this.draftSubjectSelection}
          .selectedId=${this.draftSubject?.kind === 'principal'
            ? this.draftSubject.principal.id
            : this.draftSubject?.kind === 'group_closure'
              ? this.draftSubject.groupId
              : ''}
          .selectedLabel=${this.draftSubjectLabel}
          .isSystemScope=${this.draftScopeType === 'system'}
          @subject-change=${(e: CustomEvent<SubjectChangeDetail>) => this.handleSubjectChange(e)}
        ></scion-access-boundary-subject-selector>
      </div>

      ${this.renderStepNavigation()}
    `;
  }

  // ---------------------------------------------------------------------------
  // Step 3: Scope Selection
  // ---------------------------------------------------------------------------
  private renderStep3() {
    return html`
      <h2 class="step-title">Scope</h2>
      <p class="step-description">Choose where this access boundary applies.</p>

      <div class="step-body">
        <scion-access-boundary-scope-selector
          .scopeType=${this.draftScopeType}
          .projectId=${this.draftProjectId}
          .projectLabel=${this.draftProjectLabel}
          @scope-change=${(e: CustomEvent<ScopeChangeDetail>) => this.handleScopeChange(e)}
        ></scion-access-boundary-scope-selector>
      </div>

      ${this.renderStepNavigation()}
    `;
  }

  // ---------------------------------------------------------------------------
  // Step 4: Maximum Permissions
  // ---------------------------------------------------------------------------
  private renderStep4() {
    return html`
      <h2 class="step-title">Maximum Permissions</h2>
      <p class="step-description">
        Select which permissions are retained. Permissions not selected will be removed from all
        affected principals, including any permissions registered after this boundary was last
        edited.
      </p>

      <div class="step-body">
        <scion-maximum-permission-selector
          .retainedPermissions=${this.draftRetainedPermissions}
          .newSincePermissionIds=${this.draftNewSincePermissionIds}
          @permission-change=${(e: CustomEvent<PermissionChangeDetail>) =>
            this.handlePermissionChange(e)}
        ></scion-maximum-permission-selector>
      </div>

      ${this.renderStepNavigation()}
    `;
  }

  // ---------------------------------------------------------------------------
  // Step 5: Activation Window
  // ---------------------------------------------------------------------------
  private renderStep5() {
    return html`
      <h2 class="step-title">Activation Window</h2>
      <p class="step-description">
        Optionally set a time window during which this access boundary is active.
      </p>

      <div class="step-body">
        <scion-access-boundary-schedule-editor
          .notBefore=${this.draftNotBefore}
          .expiresAt=${this.draftExpiresAt}
          @schedule-change=${(e: CustomEvent<ScheduleChangeDetail>) => this.handleScheduleChange(e)}
        ></scion-access-boundary-schedule-editor>
      </div>

      ${this.renderStepNavigation()}
    `;
  }

  // ---------------------------------------------------------------------------
  // Step 6: Review Summary + Preview/Commit
  // ---------------------------------------------------------------------------
  private renderStep6() {
    const summaryData: DefinitionSummaryData = {
      name: this.draftName,
      purpose: this.draftPurpose,
      subject: this.draftSubject,
      subjectSelection: this.draftSubjectSelection,
      subjectDisplayLabel: this.draftSubjectLabel,
      scope: this.draftScope,
      scopeDisplayLabel:
        this.draftScopeType === 'system'
          ? 'System-wide'
          : this.draftProjectLabel || this.draftProjectId,
      retainedPermissions: this.draftRetainedPermissions,
      totalPermissionCount: this.draftTotalPermissionCount,
      notBefore: this.draftNotBefore,
      expiresAt: this.draftExpiresAt,
    };

    if (this.showPreview) {
      const draft = this.buildDraft();
      return html`
        <scion-access-boundary-preview
          .draft=${draft}
          operation=${this.previewOperation}
          constraintId=${this.boundaryId}
          baseRevision=${this.baseRevision}
          autoStart
          @preview-commit-success=${(e: CustomEvent<PreviewCommitSuccessDetail>) =>
            this.handlePreviewCommitSuccess(e)}
          @preview-cancel=${() => this.handlePreviewCancel()}
        ></scion-access-boundary-preview>
      `;
    }

    return html`
      <h2 class="step-title">Review</h2>
      <p class="step-description">Review your access boundary definition before submitting.</p>

      <div class="step-body">
        <scion-access-boundary-definition-summary
          .data=${summaryData}
          @navigate-to-step=${(e: CustomEvent<number>) => this.handleSummaryNavigate(e)}
        ></scion-access-boundary-definition-summary>
      </div>

      ${this.renderStepNavigation()}
    `;
  }

  // ---------------------------------------------------------------------------
  // Step Navigation (shared across all steps)
  // ---------------------------------------------------------------------------
  private renderStepNavigation() {
    return html`
      <div class="step-navigation">
        <div class="nav-left">
          ${this.currentStep > 1
            ? html`
                <sl-button variant="default" @click=${() => this.goBack()}>
                  <sl-icon name="arrow-left" slot="prefix"></sl-icon>
                  Back
                </sl-button>
              `
            : html`
                <sl-button
                  variant="default"
                  href="/admin/access-boundaries"
                  @click=${(e: Event) => {
                    if (this.isDirty) {
                      if (!confirm('You have unsaved changes. Are you sure you want to leave?')) {
                        e.preventDefault();
                      }
                    }
                  }}
                >
                  Cancel
                </sl-button>
              `}
        </div>
        <div class="nav-right">
          ${this.currentStep < 6
            ? html`
                <sl-button variant="primary" @click=${() => this.goNext()}>
                  Next
                  <sl-icon name="arrow-right" slot="suffix"></sl-icon>
                </sl-button>
              `
            : html`
                <sl-button variant="primary" @click=${() => this.handleStartPreview()}>
                  <sl-icon name="shield-check" slot="prefix"></sl-icon>
                  Preview impact
                </sl-button>
              `}
        </div>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-admin-access-boundary-editor': ScionPageAdminAccessBoundaryEditor;
  }
}
