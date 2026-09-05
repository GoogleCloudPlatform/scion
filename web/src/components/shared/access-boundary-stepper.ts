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
 * Access Boundary Stepper — horizontal step indicator for the 6-step
 * guided authoring workflow.
 *
 * Steps: Details → Subject → Scope → Permissions → Schedule → Review
 *
 * Accessibility: uses `role="list"` with `aria-current="step"` on the active
 * step. Completed steps are clickable; future steps are inert.
 */

import { LitElement, html, css } from 'lit';
import { srOnlyStyles } from './styles.js';
import { customElement, property } from 'lit/decorators.js';
import { classMap } from 'lit/directives/class-map.js';

export interface StepDefinition {
  index: number;
  label: string;
  shortLabel: string;
}

export const BOUNDARY_STEPS: StepDefinition[] = [
  { index: 1, label: 'Details', shortLabel: 'Details' },
  { index: 2, label: 'Subject', shortLabel: 'Subject' },
  { index: 3, label: 'Scope', shortLabel: 'Scope' },
  { index: 4, label: 'Permissions', shortLabel: 'Permissions' },
  { index: 5, label: 'Schedule', shortLabel: 'Schedule' },
  { index: 6, label: 'Review', shortLabel: 'Review' },
];

@customElement('scion-access-boundary-stepper')
export class ScionAccessBoundaryStepper extends LitElement {
  /** Current active step (1-based). */
  @property({ type: Number }) currentStep = 1;

  /** Set of completed step indices (1-based). */
  @property({ type: Array }) completedSteps: number[] = [];

  static override styles = [
    srOnlyStyles,
    css`
      :host {
        display: block;
      }

      .stepper {
        display: flex;
        align-items: center;
        justify-content: center;
        gap: 0;
        padding: 1rem 0;
        list-style: none;
        margin: 0;
      }

      .step {
        display: flex;
        align-items: center;
        gap: 0;
      }

      .step-button {
        display: flex;
        align-items: center;
        gap: 0.5rem;
        padding: 0.5rem 0.75rem;
        min-height: 44px;
        min-width: 44px;
        border: none;
        background: none;
        cursor: default;
        border-radius: var(--scion-radius, 0.5rem);
        transition: background-color 0.15s ease;
        font-family: inherit;
        white-space: nowrap;
      }

      .step-button.clickable {
        cursor: pointer;
      }

      .step-button.clickable:hover {
        background: var(--scion-bg-subtle, #f1f5f9);
      }

      .step-button.clickable:focus-visible {
        outline: 2px solid var(--sl-color-primary-600, #2563eb);
        outline-offset: 2px;
      }

      .step-number {
        display: flex;
        align-items: center;
        justify-content: center;
        width: 1.75rem;
        height: 1.75rem;
        border-radius: 50%;
        font-size: 0.75rem;
        font-weight: 600;
        flex-shrink: 0;
      }

      .step-number.future {
        background: var(--scion-bg-subtle, #f1f5f9);
        color: var(--scion-text-muted, #64748b);
        border: 1px solid var(--scion-border, #e2e8f0);
      }

      .step-number.current {
        background: var(--sl-color-primary-600, #2563eb);
        color: var(--sl-color-neutral-0, #ffffff);
      }

      .step-number.completed {
        background: var(--sl-color-success-600, #16a34a);
        color: var(--sl-color-neutral-0, #ffffff);
      }

      .step-label {
        font-size: 0.8125rem;
        font-weight: 500;
        color: var(--scion-text-muted, #64748b);
      }

      .step-button.active .step-label {
        color: var(--scion-text, #1e293b);
        font-weight: 600;
      }

      .step-button.done .step-label {
        color: var(--scion-text, #1e293b);
      }

      .step-connector {
        width: 2rem;
        height: 2px;
        background: var(--scion-border, #e2e8f0);
        flex-shrink: 0;
      }

      .step-connector.completed {
        background: var(--sl-color-success-600, #16a34a);
      }

      @media (max-width: 768px) {
        .stepper {
          flex-direction: column;
          align-items: stretch;
          gap: 0;
        }

        .step {
          flex-direction: row;
          align-items: center;
        }

        .step-connector {
          width: 2px;
          height: 1.5rem;
          align-self: center;
        }

        .step-button {
          padding: 0.5rem;
          width: 100%;
          justify-content: flex-start;
        }

        .step-label {
          display: inline;
        }
      }

      @media (forced-colors: active) {
        .step-number {
          border: 2px solid ButtonText;
        }

        .step-number.current {
          border-color: Highlight;
        }

        .step-number.completed {
          border-color: ButtonText;
        }

        .step-connector {
          background: ButtonText;
        }

        .step-button.clickable:focus-visible {
          outline: 2px solid Highlight;
        }
      }

      @media (prefers-reduced-motion: reduce) {
        .step-button {
          transition: none;
        }

        .group-chevron {
          transition: none;
        }
      }
    `,
  ];

  private handleStepClick(stepIndex: number): void {
    if (this.isClickable(stepIndex)) {
      this.dispatchEvent(
        new CustomEvent<number>('step-navigate', {
          detail: stepIndex,
          bubbles: true,
          composed: true,
        })
      );
    }
  }

  private isCompleted(stepIndex: number): boolean {
    return this.completedSteps.includes(stepIndex);
  }

  private isClickable(stepIndex: number): boolean {
    return this.isCompleted(stepIndex) || stepIndex === this.currentStep;
  }

  override render() {
    return html`
      <nav aria-label="Access boundary creation steps">
        <ol class="stepper" role="list">
          ${BOUNDARY_STEPS.map((step, i) => {
            const isCurrent = step.index === this.currentStep;
            const isComplete = this.isCompleted(step.index);
            const clickable = this.isClickable(step.index);

            return html`
              ${i > 0
                ? html`<li
                    class="step-connector ${isComplete ? 'completed' : ''}"
                    aria-hidden="true"
                  ></li>`
                : ''}
              <li class="step">
                <button
                  class=${classMap({
                    'step-button': true,
                    active: isCurrent,
                    done: isComplete,
                    clickable,
                  })}
                  aria-current=${isCurrent ? 'step' : 'false'}
                  aria-label="${step.label}${isComplete ? ' (completed)' : ''}${isCurrent
                    ? ' (current)'
                    : ''}"
                  ?disabled=${!clickable}
                  tabindex=${clickable ? 0 : -1}
                  @click=${() => this.handleStepClick(step.index)}
                >
                  <span
                    class=${classMap({
                      'step-number': true,
                      current: isCurrent,
                      completed: isComplete,
                      future: !isCurrent && !isComplete,
                    })}
                  >
                    ${isComplete
                      ? html`<sl-icon name="check-lg" style="font-size: 0.875rem"></sl-icon>`
                      : step.index}
                  </span>
                  <span class="step-label">${step.label}</span>
                </button>
              </li>
            `;
          })}
        </ol>
      </nav>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-access-boundary-stepper': ScionAccessBoundaryStepper;
  }
}
