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
 * Breadcrumb Navigation Component
 *
 * Displays hierarchical navigation breadcrumbs using Shoelace
 */

import { LitElement, html, css } from 'lit';
import { customElement, property } from 'lit/decorators.js';

/**
 * Route label configuration
 */
const ROUTE_LABELS: Record<string, string> = {
  '/': 'Dashboard',
  '/groves': 'Groves',
  '/agents': 'Agents',
  '/settings': 'Settings',
};

/**
 * Breadcrumb item structure
 */
interface BreadcrumbItem {
  label: string;
  href: string;
  current: boolean;
}

@customElement('scion-breadcrumb')
export class ScionBreadcrumb extends LitElement {
  /**
   * Current path for generating breadcrumbs
   */
  @property({ type: String })
  path = '/';

  /**
   * Optional custom label for the current page
   */
  @property({ type: String })
  currentLabel = '';

  static override styles = css`
    :host {
      display: block;
    }

    sl-breadcrumb {
      --separator-color: var(--scion-text-muted, #64748b);
    }

    sl-breadcrumb-item::part(label) {
      font-size: 0.875rem;
    }

    sl-breadcrumb-item::part(label):hover {
      color: var(--scion-primary, #3b82f6);
    }

    sl-breadcrumb-item[aria-current='page']::part(label) {
      color: var(--scion-text, #1e293b);
      font-weight: 500;
    }

    .breadcrumb-icon {
      font-size: 0.875rem;
      vertical-align: middle;
      margin-right: 0.25rem;
    }
  `;

  override render() {
    const items = this.generateBreadcrumbs();

    if (items.length <= 1) {
      // Don't show breadcrumbs for root level
      return html``;
    }

    return html`
      <sl-breadcrumb>
        ${items.map(
          (item, index) => html`
            <sl-breadcrumb-item
              href="${item.current ? '' : item.href}"
              ?aria-current=${item.current ? 'page' : false}
            >
              ${index === 0 ? html`<sl-icon name="house" class="breadcrumb-icon"></sl-icon>` : ''}
              ${item.label}
            </sl-breadcrumb-item>
          `
        )}
      </sl-breadcrumb>
    `;
  }

  /**
   * Generate breadcrumb items from the current path
   */
  private generateBreadcrumbs(): BreadcrumbItem[] {
    const items: BreadcrumbItem[] = [];

    // Always start with home
    items.push({
      label: 'Home',
      href: '/',
      current: this.path === '/',
    });

    if (this.path === '/') {
      return items;
    }

    // Parse the path and build breadcrumbs
    const segments = this.path.split('/').filter(Boolean);
    let currentPath = '';

    segments.forEach((segment, index) => {
      currentPath += `/${segment}`;
      const isLast = index === segments.length - 1;

      // Get label from configuration or format the segment
      let label = ROUTE_LABELS[currentPath];

      if (!label) {
        // Check if this is a dynamic segment (like an ID)
        if (this.isId(segment)) {
          // Use custom label for IDs or truncate
          label = this.currentLabel && isLast ? this.currentLabel : this.formatId(segment);
        } else {
          label = this.formatSegment(segment);
        }
      }

      items.push({
        label,
        href: currentPath,
        current: isLast,
      });
    });

    return items;
  }

  /**
   * Check if a segment looks like an ID
   */
  private isId(segment: string): boolean {
    // UUID pattern or numeric ID
    return /^[0-9a-f-]{8,}$/i.test(segment) || /^\d+$/.test(segment);
  }

  /**
   * Format an ID segment for display
   */
  private formatId(id: string): string {
    // Show first 8 characters of UUID or full numeric ID
    if (id.length > 8) {
      return id.slice(0, 8) + '...';
    }
    return id;
  }

  /**
   * Format a path segment as a label
   */
  private formatSegment(segment: string): string {
    return segment
      .split('-')
      .map((word) => word.charAt(0).toUpperCase() + word.slice(1))
      .join(' ');
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-breadcrumb': ScionBreadcrumb;
  }
}
