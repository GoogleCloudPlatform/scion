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
 * Styles shared by the Brokers area pages (broker UI Option A, slice A1).
 * Copied from the existing broker detail page, which stays unchanged while
 * the hub.flat_runtime_brokers experiment is off.
 */

import { css } from 'lit';

export const brokerAreaStyles = css`
  :host {
    display: block;
  }

  .header-subtitle {
    font-family: var(--scion-font-mono, monospace);
    font-size: 0.875rem;
    color: var(--scion-text-muted, #64748b);
    margin-top: 0.25rem;
    word-break: break-all;
  }

  .host-identity-alert {
    display: block;
    margin-bottom: 1rem;
  }

  .stats-row {
    display: flex;
    gap: 2rem;
    margin-bottom: 2rem;
    padding: 1.25rem;
    background: var(--scion-surface, #ffffff);
    border: 1px solid var(--scion-border, #e2e8f0);
    border-radius: var(--scion-radius-lg, 0.75rem);
    flex-wrap: wrap;
  }

  .stat {
    display: flex;
    flex-direction: column;
  }

  .stat-label {
    font-size: 0.75rem;
    color: var(--scion-text-muted, #64748b);
    text-transform: uppercase;
    letter-spacing: 0.05em;
    margin-bottom: 0.25rem;
  }

  .stat-value {
    font-size: 1.5rem;
    font-weight: 700;
    color: var(--scion-text, #1e293b);
  }

  .stat-value-sm {
    font-size: 1rem;
    font-weight: 500;
    color: var(--scion-text, #1e293b);
  }

  .section {
    margin-bottom: 2rem;
  }

  .section-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-bottom: 1rem;
  }

  .section-header h2 {
    font-size: 1.125rem;
    font-weight: 600;
    color: var(--scion-text, #1e293b);
    margin: 0;
  }

  .runtime-target-card {
    padding: 1.25rem;
    background: var(--scion-surface, #ffffff);
    border: 1px solid var(--scion-border, #e2e8f0);
    border-radius: var(--scion-radius-lg, 0.75rem);
  }

  .runtime-target-header {
    justify-content: flex-start;
    gap: 0.5rem;
  }

  .runtime-target-card h3 {
    font-size: 0.875rem;
    font-weight: 600;
    color: var(--scion-text, #1e293b);
    margin: 1rem 0 0.5rem;
  }

  .runtime-target-rows {
    display: grid;
    grid-template-columns: max-content 1fr;
    gap: 0.25rem 1rem;
    margin: 0;
    font-size: 0.875rem;
  }

  .runtime-target-rows dt {
    color: var(--scion-text-muted, #64748b);
  }

  .runtime-target-rows dd {
    margin: 0;
    color: var(--scion-text, #1e293b);
    word-break: break-word;
  }

  .presentation-caption {
    font-size: 0.8125rem;
    color: var(--scion-text-muted, #64748b);
    margin: 0.75rem 0 0;
  }

  .capabilities-row {
    display: flex;
    flex-wrap: wrap;
    gap: 0.5rem;
    margin-bottom: 2rem;
  }

  .capability-tag {
    display: inline-flex;
    align-items: center;
    gap: 0.25rem;
    padding: 0.25rem 0.5rem;
    border-radius: var(--scion-radius, 0.5rem);
    font-size: 0.75rem;
    font-weight: 500;
    background: var(--scion-bg-subtle, #f1f5f9);
    color: var(--scion-text-muted, #64748b);
  }

  .capability-tag.enabled {
    background: var(--sl-color-success-100, #dcfce7);
    color: var(--sl-color-success-700, #15803d);
  }

  .profiles-row {
    display: flex;
    flex-wrap: wrap;
    gap: 0.5rem;
    margin-bottom: 2rem;
  }

  .profile-tag {
    display: inline-flex;
    align-items: center;
    gap: 0.375rem;
    padding: 0.375rem 0.75rem;
    border-radius: var(--scion-radius, 0.5rem);
    font-size: 0.8125rem;
    font-weight: 500;
    background: var(--scion-bg-subtle, #f1f5f9);
    color: var(--scion-text-muted, #64748b);
    border: 1px solid var(--scion-border, #e2e8f0);
  }

  .profile-tag.available {
    background: var(--scion-surface, #ffffff);
    color: var(--scion-text, #1e293b);
  }

  .profile-type {
    font-size: 0.6875rem;
    text-transform: uppercase;
    letter-spacing: 0.05em;
    opacity: 0.7;
  }

  .project-section {
    margin-bottom: 1.5rem;
  }

  .project-section-header {
    display: flex;
    align-items: center;
    gap: 0.75rem;
    margin-bottom: 1rem;
    padding-bottom: 0.5rem;
    border-bottom: 1px solid var(--scion-border, #e2e8f0);
  }

  .project-section-header h3 {
    font-size: 1rem;
    font-weight: 600;
    color: var(--scion-text, #1e293b);
    margin: 0;
  }

  .project-section-header a {
    color: inherit;
    text-decoration: none;
  }

  .project-section-header a:hover {
    color: var(--scion-primary, #3b82f6);
  }

  .project-section-header sl-icon {
    color: var(--scion-primary, #3b82f6);
  }

  .project-agent-count {
    font-size: 0.75rem;
    color: var(--scion-text-muted, #64748b);
    background: var(--scion-bg-subtle, #f1f5f9);
    padding: 0.125rem 0.5rem;
    border-radius: 9999px;
  }

  .agent-grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(320px, 1fr));
    gap: 1.5rem;
  }

  .agent-card {
    background: var(--scion-surface, #ffffff);
    border: 1px solid var(--scion-border, #e2e8f0);
    border-radius: var(--scion-radius-lg, 0.75rem);
    padding: 1.5rem;
    transition: all var(--scion-transition-fast, 150ms ease);
    text-decoration: none;
    color: inherit;
    display: block;
  }

  .agent-card:hover {
    border-color: var(--scion-primary, #3b82f6);
    box-shadow: var(--scion-shadow-md, 0 4px 6px -1px rgba(0, 0, 0, 0.1));
  }

  .agent-header {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    margin-bottom: 0.75rem;
    gap: 0.5rem;
    flex-wrap: wrap;
  }

  /* Full-width basis: the status badge always wraps to its own row. */
  .agent-header > div {
    flex: 1 1 100%;
    min-width: 0;
  }

  .agent-header > scion-status-badge {
    flex-shrink: 0;
  }

  .agent-name {
    font-size: 1.125rem;
    font-weight: 600;
    color: var(--scion-text, #1e293b);
    margin: 0;
    display: flex;
    align-items: center;
    gap: 0.5rem;
    min-width: 0;
  }

  .agent-name sl-icon {
    color: var(--scion-primary, #3b82f6);
    flex-shrink: 0;
  }

  /* The span wrapper lets the name shrink and wrap. */
  .agent-name > span {
    min-width: 0;
    overflow-wrap: anywhere;
  }

  .agent-meta {
    font-size: 0.813rem;
    color: var(--scion-text-muted, #64748b);
    margin-top: 0.25rem;
  }

  .agent-meta sl-icon {
    font-size: 0.875rem;
    vertical-align: -0.125em;
    opacity: 0.7;
  }

  .agent-task {
    font-size: 0.875rem;
    color: var(--scion-text, #1e293b);
    margin-top: 0.75rem;
    padding: 0.75rem;
    background: var(--scion-bg-subtle, #f1f5f9);
    border-radius: var(--scion-radius, 0.5rem);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .empty-state {
    text-align: center;
    padding: 4rem 2rem;
    background: var(--scion-surface, #ffffff);
    border: 1px dashed var(--scion-border, #e2e8f0);
    border-radius: var(--scion-radius-lg, 0.75rem);
  }

  .empty-state > sl-icon {
    font-size: 4rem;
    color: var(--scion-text-muted, #64748b);
    opacity: 0.5;
    margin-bottom: 1rem;
  }

  .empty-state h2 {
    font-size: 1.25rem;
    font-weight: 600;
    color: var(--scion-text, #1e293b);
    margin: 0 0 0.5rem 0;
  }

  .empty-state p {
    color: var(--scion-text-muted, #64748b);
    margin: 0;
  }

  .loading-state {
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    padding: 4rem 2rem;
    color: var(--scion-text-muted, #64748b);
  }

  .loading-state sl-spinner {
    font-size: 2rem;
    margin-bottom: 1rem;
  }

  .error-state {
    text-align: center;
    padding: 3rem 2rem;
    background: var(--scion-surface, #ffffff);
    border: 1px solid var(--sl-color-danger-200, #fecaca);
    border-radius: var(--scion-radius-lg, 0.75rem);
  }

  .error-state sl-icon {
    font-size: 3rem;
    color: var(--sl-color-danger-500, #ef4444);
    margin-bottom: 1rem;
  }

  .error-state h2 {
    font-size: 1.25rem;
    font-weight: 600;
    color: var(--scion-text, #1e293b);
    margin: 0 0 0.5rem 0;
  }

  .error-state p {
    color: var(--scion-text-muted, #64748b);
    margin: 0 0 1rem 0;
  }

  .error-details {
    font-family: var(--scion-font-mono, monospace);
    font-size: 0.875rem;
    background: var(--scion-bg-subtle, #f1f5f9);
    padding: 0.75rem 1rem;
    border-radius: var(--scion-radius, 0.5rem);
    color: var(--sl-color-danger-700, #b91c1c);
    margin-bottom: 1rem;
  }

  .tab-note {
    font-size: 0.8125rem;
    color: var(--scion-text-muted, #64748b);
    margin: 0.5rem 0 0;
  }

  .hint {
    font-size: 0.75rem;
    color: var(--scion-text-muted, #64748b);
    margin: 0.25rem 0 0.75rem;
  }

  .form-error {
    margin-bottom: 0.75rem;
    font-size: 0.8125rem;
    color: var(--scion-danger, #dc2626);
  }

  .hosted-marker {
    color: var(--scion-primary, #3b82f6);
    vertical-align: -0.125em;
  }

  .kind-badge {
    display: inline-block;
    padding: 0.0625rem 0.5rem;
    border-radius: 9999px;
    font-size: 0.6875rem;
    font-weight: 600;
    background: var(--scion-bg-subtle, #f1f5f9);
    color: var(--scion-text-muted, #64748b);
  }

  .kind-badge.flat {
    background: var(--sl-color-primary-100, #dbeafe);
    color: var(--sl-color-primary-700, #1d4ed8);
  }
`;
