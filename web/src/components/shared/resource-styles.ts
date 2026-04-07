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
 * Shared CSS styles for env var and secret list components.
 *
 * Covers table, dialog, badge, empty/loading/error states, and
 * compact section layout used by grove-settings.
 */

import { css } from 'lit';

export const resourceStyles = css`
  :host {
    display: block;
  }

  /* ── Table ──────────────────────────────────────────────────────────── */

  .table-container {
    background: var(--scion-surface, #ffffff);
    border: 1px solid var(--scion-border, #e2e8f0);
    border-radius: var(--scion-radius-lg, 0.75rem);
    overflow: hidden;
  }

  table {
    width: 100%;
    border-collapse: collapse;
  }

  th {
    text-align: left;
    padding: 0.75rem 1rem;
    font-size: 0.75rem;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.05em;
    color: var(--scion-text-muted, #64748b);
    background: var(--scion-bg-subtle, #f1f5f9);
    border-bottom: 1px solid var(--scion-border, #e2e8f0);
  }

  td {
    padding: 0.75rem 1rem;
    font-size: 0.875rem;
    color: var(--scion-text, #1e293b);
    border-bottom: 1px solid var(--scion-border, #e2e8f0);
    vertical-align: middle;
  }

  tr:last-child td {
    border-bottom: none;
  }

  tr:hover td {
    background: var(--scion-bg-subtle, #f1f5f9);
  }

  .key-cell {
    font-family: var(--scion-font-mono, monospace);
    font-weight: 600;
    font-size: 0.8125rem;
  }

  .value-cell {
    font-family: var(--scion-font-mono, monospace);
    font-size: 0.8125rem;
    max-width: 300px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .description-cell {
    max-width: 200px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    color: var(--scion-text-muted, #64748b);
  }

  .actions-cell {
    text-align: right;
    white-space: nowrap;
  }

  .meta-text {
    font-size: 0.8125rem;
    color: var(--scion-text-muted, #64748b);
  }

  /* ── Badges ─────────────────────────────────────────────────────────── */

  .badge {
    display: inline-flex;
    align-items: center;
    gap: 0.25rem;
    padding: 0.125rem 0.5rem;
    border-radius: 9999px;
    font-size: 0.6875rem;
    font-weight: 500;
  }

  .badge.sensitive {
    background: var(--sl-color-warning-100, #fef3c7);
    color: var(--sl-color-warning-700, #b45309);
  }

  .badge.secret {
    background: var(--sl-color-danger-100, #fee2e2);
    color: var(--sl-color-danger-700, #b91c1c);
  }

  .badge.inject-always {
    background: var(--sl-color-primary-100, #dbeafe);
    color: var(--sl-color-primary-700, #1d4ed8);
  }

  .badge.inject-as-needed {
    background: var(--scion-bg-subtle, #f1f5f9);
    color: var(--scion-text-muted, #64748b);
  }

  .badges {
    display: flex;
    gap: 0.375rem;
    flex-wrap: wrap;
  }

  /* ── Secret-specific badges ─────────────────────────────────────────── */

  .key-info {
    display: flex;
    align-items: center;
    gap: 0.5rem;
  }

  .key-icon {
    width: 1.75rem;
    height: 1.75rem;
    border-radius: 0.375rem;
    display: flex;
    align-items: center;
    justify-content: center;
    flex-shrink: 0;
    background: var(--sl-color-danger-100, #fee2e2);
    color: var(--sl-color-danger-600, #dc2626);
  }

  .key-icon sl-icon {
    font-size: 0.875rem;
  }

  .type-badge {
    display: inline-flex;
    align-items: center;
    padding: 0.125rem 0.5rem;
    border-radius: 9999px;
    font-size: 0.6875rem;
    font-weight: 500;
  }

  .type-badge.environment {
    background: var(--sl-color-primary-100, #dbeafe);
    color: var(--sl-color-primary-700, #1d4ed8);
  }

  .type-badge.variable {
    background: var(--sl-color-success-100, #dcfce7);
    color: var(--sl-color-success-700, #15803d);
  }

  .type-badge.file {
    background: var(--sl-color-warning-100, #fef3c7);
    color: var(--sl-color-warning-700, #b45309);
  }

  .version-badge {
    display: inline-flex;
    align-items: center;
    padding: 0.125rem 0.5rem;
    border-radius: 9999px;
    font-size: 0.6875rem;
    font-weight: 500;
    background: var(--scion-bg-subtle, #f1f5f9);
    color: var(--scion-text-muted, #64748b);
    font-family: var(--scion-font-mono, monospace);
  }

  .version-badge-copyable {
    cursor: pointer;
  }

  .version-badge-copyable:hover {
    background: var(--scion-bg-hover, #e2e8f0);
  }

  /* ── Empty / Loading / Error states ─────────────────────────────────── */

  .empty-state {
    text-align: center;
    padding: 3rem 2rem;
    background: var(--scion-surface, #ffffff);
    border: 1px dashed var(--scion-border, #e2e8f0);
    border-radius: var(--scion-radius-lg, 0.75rem);
  }

  .empty-state > sl-icon {
    font-size: 3rem;
    color: var(--scion-text-muted, #64748b);
    opacity: 0.5;
    margin-bottom: 0.75rem;
  }

  .empty-state h3 {
    font-size: 1.125rem;
    font-weight: 600;
    color: var(--scion-text, #1e293b);
    margin: 0 0 0.5rem 0;
  }

  .empty-state p {
    color: var(--scion-text-muted, #64748b);
    margin: 0 0 1.25rem 0;
    font-size: 0.875rem;
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

  /* ── Compact section layout (grove page) ────────────────────────────── */

  .section {
    background: var(--scion-surface, #ffffff);
    border: 1px solid var(--scion-border, #e2e8f0);
    border-radius: var(--scion-radius-lg, 0.75rem);
    padding: 1.5rem;
    margin-bottom: 1.5rem;
  }

  .section-header {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    margin-bottom: 1rem;
    gap: 1rem;
  }

  .section-header-info h2 {
    font-size: 1.125rem;
    font-weight: 600;
    color: var(--scion-text, #1e293b);
    margin: 0 0 0.25rem 0;
  }

  .section-header-info p {
    color: var(--scion-text-muted, #64748b);
    font-size: 0.875rem;
    margin: 0;
  }

  .section-loading {
    display: flex;
    align-items: center;
    justify-content: center;
    padding: 2rem;
    color: var(--scion-text-muted, #64748b);
    gap: 0.75rem;
  }

  .section-error {
    color: var(--sl-color-danger-600, #dc2626);
    font-size: 0.875rem;
    padding: 0.75rem 1rem;
    background: var(--sl-color-danger-50, #fef2f2);
    border-radius: var(--scion-radius, 0.5rem);
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 0.5rem;
  }

  /* ── Compact empty state (smaller for sections) ─────────────────────── */

  .compact .empty-state {
    padding: 2rem 1.5rem;
  }

  .compact .empty-state > sl-icon {
    font-size: 2.5rem;
    margin-bottom: 0.5rem;
  }

  .compact .empty-state h3 {
    font-size: 1rem;
    margin: 0 0 0.375rem 0;
  }

  /* ── Non-compact header (add button above table) ────────────────────── */

  .list-header {
    display: flex;
    justify-content: flex-end;
    margin-bottom: 1rem;
  }

  /* ── Dialog form ────────────────────────────────────────────────────── */

  .dialog-form {
    display: flex;
    flex-direction: column;
    gap: 1rem;
  }

  .dialog-error {
    color: var(--sl-color-danger-600, #dc2626);
    font-size: 0.875rem;
    padding: 0.5rem 0.75rem;
    background: var(--sl-color-danger-50, #fef2f2);
    border-radius: var(--scion-radius, 0.5rem);
  }

  .dialog-hint {
    font-size: 0.8125rem;
    color: var(--scion-text-muted, #64748b);
    padding: 0.625rem 0.75rem;
    background: var(--sl-color-warning-50, #fffbeb);
    border: 1px solid var(--sl-color-warning-200, #fde68a);
    border-radius: var(--scion-radius, 0.5rem);
    display: flex;
    align-items: center;
    gap: 0.5rem;
  }

  .dialog-hint sl-icon {
    flex-shrink: 0;
    color: var(--sl-color-warning-600, #d97706);
  }

  /* ── Checkbox group ─────────────────────────────────────────────────── */

  .checkbox-group {
    display: flex;
    flex-direction: column;
    gap: 0.75rem;
  }

  .checkbox-label {
    display: flex;
    align-items: flex-start;
    gap: 0.5rem;
    cursor: pointer;
    font-size: 0.875rem;
    color: var(--scion-text, #1e293b);
  }

  .checkbox-label input[type='checkbox'] {
    margin-top: 0.125rem;
    flex-shrink: 0;
  }

  .checkbox-text {
    display: flex;
    flex-direction: column;
  }

  .checkbox-description {
    font-size: 0.75rem;
    color: var(--scion-text-muted, #64748b);
    margin-top: 0.125rem;
  }

  /* ── Radio field ────────────────────────────────────────────────────── */

  .radio-field {
    display: flex;
    flex-direction: column;
    gap: 0.375rem;
  }

  .radio-field-label {
    font-size: 0.875rem;
    font-weight: 500;
    color: var(--scion-text, #1e293b);
  }

  .radio-field-help {
    font-size: 0.75rem;
    color: var(--scion-text-muted, #64748b);
  }

  /* ── Responsive ─────────────────────────────────────────────────────── */

  @media (max-width: 768px) {
    .hide-mobile {
      display: none;
    }
  }
`;

/**
 * Shared CSS styles for resource list pages (groves, agents, brokers).
 *
 * Consolidates duplicated header, loading/error/empty state, card grid,
 * table, and stat styles so each page only needs page-specific overrides.
 */
export const listPageStyles = css`
  :host {
    display: block;
  }

  /* ── Page header ─────────────────────────────────────────────────── */

  .header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-bottom: 1.5rem;
  }

  .header h1 {
    font-size: 1.5rem;
    font-weight: 700;
    color: var(--scion-text, #1e293b);
    margin: 0;
  }

  .header-actions {
    display: flex;
    align-items: center;
    gap: 0.75rem;
  }

  /* ── Card grid ───────────────────────────────────────────────────── */

  .resource-grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(320px, 1fr));
    gap: 1.5rem;
  }

  .resource-card {
    background: var(--scion-surface, #ffffff);
    border: 1px solid var(--scion-border, #e2e8f0);
    border-radius: var(--scion-radius-lg, 0.75rem);
    padding: 1.5rem;
    transition: all var(--scion-transition-fast, 150ms ease);
    cursor: pointer;
    text-decoration: none;
    color: inherit;
    display: block;
  }

  .resource-card:hover {
    border-color: var(--scion-primary, #3b82f6);
    box-shadow: var(--scion-shadow-md, 0 4px 6px -1px rgba(0, 0, 0, 0.1));
    transform: translateY(-2px);
  }

  /* ── Resource name (icon + text) ─────────────────────────────────── */

  .resource-name {
    font-size: 1.125rem;
    font-weight: 600;
    color: var(--scion-text, #1e293b);
    margin: 0;
    display: flex;
    align-items: center;
    gap: 0.5rem;
  }

  .resource-name sl-icon {
    color: var(--scion-primary, #3b82f6);
  }

  /* ── Stats (label + value pairs) ─────────────────────────────────── */

  .stat {
    display: flex;
    flex-direction: column;
  }

  .stat-label {
    font-size: 0.75rem;
    color: var(--scion-text-muted, #64748b);
    text-transform: uppercase;
    letter-spacing: 0.05em;
  }

  .stat-value {
    font-size: 0.875rem;
    font-weight: 500;
    color: var(--scion-text, #1e293b);
  }

  /* ── Table layout ────────────────────────────────────────────────── */

  .resource-table-container {
    background: var(--scion-surface, #ffffff);
    border: 1px solid var(--scion-border, #e2e8f0);
    border-radius: var(--scion-radius-lg, 0.75rem);
    overflow: hidden;
  }

  .resource-table-container table {
    width: 100%;
    border-collapse: collapse;
  }

  .resource-table-container th {
    text-align: left;
    padding: 0.75rem 1rem;
    font-size: 0.75rem;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.05em;
    color: var(--scion-text-muted, #64748b);
    background: var(--scion-bg-subtle, #f1f5f9);
    border-bottom: 1px solid var(--scion-border, #e2e8f0);
  }

  .resource-table-container td {
    padding: 0.75rem 1rem;
    font-size: 0.875rem;
    color: var(--scion-text, #1e293b);
    border-bottom: 1px solid var(--scion-border, #e2e8f0);
    vertical-align: middle;
  }

  .resource-table-container tr:last-child td {
    border-bottom: none;
  }

  .resource-table-container tr:hover td {
    background: var(--scion-bg-subtle, #f1f5f9);
  }

  .resource-table-container tr.clickable {
    cursor: pointer;
  }

  .resource-table-container .actions-cell {
    text-align: right;
    white-space: nowrap;
  }

  .resource-table-container .mono-cell {
    font-family: var(--scion-font-mono, monospace);
    font-size: 0.8125rem;
    max-width: 300px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .resource-table-container .name-cell {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    font-weight: 500;
  }

  .resource-table-container .name-cell sl-icon {
    color: var(--scion-primary, #3b82f6);
    flex-shrink: 0;
  }

  .resource-table-container .name-cell a {
    color: inherit;
    text-decoration: none;
  }

  .resource-table-container .name-cell a:hover {
    text-decoration: underline;
  }

  .resource-table-container .status-col {
    min-width: 11rem;
  }

  .resource-table-container .task-cell {
    max-width: 250px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    color: var(--scion-text-muted, #64748b);
    font-size: 0.8125rem;
  }

  .resource-table-container .meta-text {
    font-size: 0.8125rem;
    color: var(--scion-text-muted, #64748b);
  }

  /* ── Loading / Error / Empty states ──────────────────────────────── */

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
    margin: 0 0 1.5rem 0;
  }

  /* ── Responsive ──────────────────────────────────────────────────── */

  @media (max-width: 768px) {
    .hide-mobile {
      display: none;
    }
  }
`;
