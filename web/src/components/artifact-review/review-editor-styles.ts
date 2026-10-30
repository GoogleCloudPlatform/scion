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
 * Styles of the review editor, adopted in its shadow root: ProseMirror's
 * base rules, the document, and the marks. Mark colours come from the
 * --scion-review-* theme tokens (theme.css), each a text colour on its own
 * background, so marks keep their contrast in both themes (the fallbacks
 * are the light values); insertions are also underlined, deletions
 * struck through, and comments carry a numbered badge. Task-list items
 * show their checkbox (display only), and strikethrough is a plain line
 * through, apart from a suggested deletion.
 */

import { css } from 'lit';

export const reviewEditorStyles = css`
  .visually-hidden {
    position: absolute;
    width: 1px;
    height: 1px;
    overflow: hidden;
    clip: rect(0 0 0 0);
    white-space: nowrap;
  }

  /* ProseMirror base rules (prosemirror-view/style/prosemirror.css). */
  .ProseMirror {
    position: relative;
    word-wrap: break-word;
    white-space: pre-wrap;
    white-space: break-spaces;
    font-variant-ligatures: none;
    font-feature-settings: 'liga' 0;
  }
  .ProseMirror pre {
    white-space: pre-wrap;
  }
  .ProseMirror li {
    position: relative;
  }
  .ProseMirror-hideselection *::selection {
    background: transparent;
  }
  .ProseMirror-hideselection {
    caret-color: transparent;
  }
  .ProseMirror [draggable][contenteditable='false'] {
    user-select: text;
  }
  img.ProseMirror-separator {
    display: inline !important;
    border: none !important;
    margin: 0 !important;
  }

  /* The document. */
  .ProseMirror {
    outline: none;
    max-width: 60rem;
    padding: 1rem 2rem 2rem;
    min-height: 20rem;
    font-size: 0.9375rem;
    line-height: 1.65;
    color: var(--scion-text, #0f172a);
    counter-reset: critic-note;
  }
  .ProseMirror:focus-visible {
    outline: 2px solid var(--sl-color-primary-500, #0ea5e9);
    outline-offset: -2px;
    border-radius: 0.25rem;
  }
  .ProseMirror h1,
  .ProseMirror h2,
  .ProseMirror h3,
  .ProseMirror h4,
  .ProseMirror h5,
  .ProseMirror h6 {
    margin: 1.3rem 0 0.4rem;
    font-weight: 600;
    line-height: 1.3;
  }
  .ProseMirror > :first-child {
    margin-top: 0.2rem;
  }
  .ProseMirror h1 {
    font-size: 1.6rem;
  }
  .ProseMirror h2 {
    font-size: 1.2rem;
    padding-bottom: 0.2rem;
    border-bottom: 1px solid var(--scion-border, #e2e8f0);
  }
  .ProseMirror p {
    margin: 0 0 0.85em;
  }
  .ProseMirror ul,
  .ProseMirror ol {
    margin: 0 0 0.85em;
    padding-left: 1.6rem;
  }
  .ProseMirror li {
    margin: 0.15rem 0;
  }
  .ProseMirror li > p {
    margin: 0;
  }
  /*
   * Task-list checkboxes, drawn from the item's own attributes: a box is
   * not text, takes no input, and its state is read out before the item.
   * An unticked item may carry no data-checked at all.
   */
  .ProseMirror li[data-item-type='task'] {
    list-style: none;
  }
  .ProseMirror li[data-item-type='task']::before {
    content: '☐';
    content: '☐' / 'Not done: ';
    position: absolute;
    left: -1.35em;
    top: 0;
    width: 1.2em;
    text-align: center;
    color: var(--scion-text-muted, #64748b);
    user-select: none;
  }
  .ProseMirror li[data-item-type='task'][data-checked='true']::before {
    content: '☑';
    content: '☑' / 'Done: ';
    color: var(--scion-text, #0f172a);
  }
  .ProseMirror a {
    color: var(--sl-color-primary-600, #0284c7);
    text-decoration: underline;
    text-underline-offset: 0.15em;
  }
  .ProseMirror a:not([href]) {
    color: inherit;
    text-decoration-style: dotted;
  }
  .ProseMirror code {
    font-family: var(--scion-font-mono, ui-monospace, monospace);
    font-size: 0.85em;
    background: var(--scion-bg-subtle, #f1f5f9);
    border: 1px solid var(--scion-border, #e2e8f0);
    padding: 0.1em 0.35em;
    border-radius: 0.25rem;
  }
  .ProseMirror pre {
    position: relative;
    background: var(--scion-bg-subtle, #f1f5f9);
    border: 1px solid var(--scion-border, #e2e8f0);
    border-radius: 0.5rem;
    padding: 0.75rem 1rem;
    font-size: 0.8125rem;
    margin: 0 0 1em;
    overflow-x: auto;
  }
  .ProseMirror pre code {
    background: none;
    border: none;
    padding: 0;
  }
  /* Code is edited in the source view; say so on hover and focus. */
  .ProseMirror pre::after {
    content: 'Edit in Source view';
    position: absolute;
    right: 0.5rem;
    top: 0.35rem;
    font-family: var(--sl-font-sans, system-ui, sans-serif);
    font-size: 0.6875rem;
    color: var(--scion-text-muted, #64748b);
    opacity: 0;
    transition: opacity 0.15s;
    pointer-events: none;
  }
  .ProseMirror pre:hover::after,
  .ProseMirror pre:focus-within::after {
    opacity: 1;
  }
  .ProseMirror code.raw-html {
    color: var(--scion-text-muted, #64748b);
  }
  .ProseMirror blockquote {
    border-left: 4px solid var(--scion-border, #cbd5e1);
    margin: 0 0 1em;
    padding: 0.4em 1em;
    color: var(--scion-text-muted, #64748b);
    background: var(--scion-bg-subtle, #f8fafc);
  }
  .ProseMirror table {
    border-collapse: collapse;
    margin: 0 0 1em;
  }
  .ProseMirror th,
  .ProseMirror td {
    border: 1px solid var(--scion-border, #e2e8f0);
    padding: 0.4em 0.7em;
    text-align: left;
  }
  .ProseMirror hr {
    border: none;
    border-top: 1px solid var(--scion-border, #e2e8f0);
    margin: 1.5em 0;
  }
  .ProseMirror img {
    max-width: 100%;
    height: auto;
  }
  .ProseMirror .image-placeholder {
    display: inline-flex;
    align-items: center;
    gap: 0.4rem;
    padding: 0.25em 0.6em;
    border: 1px dashed var(--scion-border, #cbd5e1);
    border-radius: 0.3rem;
    color: var(--scion-text-muted, #64748b);
    background: var(--scion-bg-subtle, #f8fafc);
    font-size: 0.8125rem;
  }
  .ProseMirror .image-placeholder.failed {
    border-color: var(--sl-color-danger-300, #fca5a5);
    color: var(--sl-color-danger-800, #991b1b);
    background: var(--sl-color-danger-50, #fef2f2);
  }
  .ProseMirror-selectednode {
    outline: 2px solid var(--sl-color-primary-500, #0ea5e9);
    outline-offset: 1px;
  }

  /* Marks. */
  s.gfm-strike {
    color: inherit;
    background: none;
    text-decoration: line-through;
    text-decoration-color: currentColor;
  }
  ins.critic-ins,
  ins.critic-sub-new {
    color: var(--scion-review-ins-text, #166534);
    background: var(--scion-review-ins-bg, #dcfce7);
    text-decoration: underline;
    text-decoration-color: var(--scion-review-ins-line, #16a34a);
    text-underline-offset: 0.18em;
    border-radius: 0.15rem;
  }
  del.critic-del,
  del.critic-sub-old {
    color: var(--scion-review-del-text, #991b1b);
    background: var(--scion-review-del-bg, #fee2e2);
    text-decoration: line-through;
    text-decoration-color: var(--scion-review-del-line, #dc2626);
    border-radius: 0.15rem;
  }
  mark.critic-hl {
    background: var(--scion-review-hl-bg, #fef08a);
    color: var(--scion-review-hl-text, #0f172a);
    border-bottom: 2px solid var(--scion-review-hl-line, #ca8a04);
    border-radius: 0.15rem;
  }
  .critic-comment {
    counter-increment: critic-note;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    gap: 0.15rem;
    font-size: 0.6875rem;
    font-weight: 700;
    color: var(--scion-review-note-text, #6d28d9);
    background: var(--scion-review-note-bg, #ede9fe);
    border: 1px solid var(--scion-review-note-line, #8b5cf6);
    border-radius: 0.6rem 0.6rem 0.6rem 0.1rem;
    min-width: 1.15rem;
    height: 1.05rem;
    padding: 0 0.25rem;
    margin: 0 0.15rem;
    vertical-align: 0.25em;
    line-height: 1;
    cursor: pointer;
  }
  .critic-comment::after {
    content: counter(critic-note);
  }
  .critic-comment sl-icon {
    font-size: 0.625rem;
  }
  .critic-comment:focus-visible {
    outline: 2px solid var(--sl-color-primary-500, #0ea5e9);
    outline-offset: 1px;
  }
  @media (forced-colors: active) {
    ins.critic-ins,
    ins.critic-sub-new {
      text-decoration: underline;
    }
    del.critic-del,
    del.critic-sub-old {
      text-decoration: line-through;
    }
    .critic-comment {
      border: 1px solid CanvasText;
    }
    .ProseMirror li[data-item-type='task']::before {
      color: CanvasText;
    }
  }

  /* Comment card (read-only: it never takes the pointer). */
  sl-popup {
    pointer-events: none;
  }
  .card {
    width: 18rem;
    max-width: calc(100vw - 2rem);
    background: var(--scion-surface, #fff);
    color: var(--scion-text, #0f172a);
    border: 1px solid var(--scion-review-note-line, #8b5cf6);
    border-radius: 0.45rem;
    box-shadow: var(--sl-shadow-large, 0 10px 30px rgba(15, 23, 42, 0.25));
    padding: 0.55rem 0.7rem;
    font-size: 0.8125rem;
    line-height: 1.45;
  }
  .card-title {
    display: flex;
    gap: 0.35rem;
    align-items: center;
    font-size: 0.75rem;
    font-weight: 600;
    color: var(--scion-text-muted, #64748b);
    margin-bottom: 0.2rem;
  }
  .card-note {
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
`;
