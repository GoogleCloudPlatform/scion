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
 * Code Editor Component
 *
 * Thin wrapper around CodeMirror 6 that provides syntax-highlighted editing.
 * The CodeMirror bundle is lazily loaded on first use.
 */

import { LitElement, html, css } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
// Type-only imports: erased at build time, so CodeMirror stays in its own
// lazily loaded chunk.
import type * as CMView from '@codemirror/view';
import type { EditorView } from '@codemirror/view';
import type * as CMState from '@codemirror/state';
import type { Compartment, Extension } from '@codemirror/state';
import type * as CMCommands from '@codemirror/commands';
import type * as CMLanguage from '@codemirror/language';
import type * as CMSearch from '@codemirror/search';
import type * as CMAutocomplete from '@codemirror/autocomplete';

// ────────────────────────────────────────────────────────────
// Language mode mapping
// ────────────────────────────────────────────────────────────

/** Map file extensions to CodeMirror language identifiers. */
const EXTENSION_LANGUAGE_MAP: Record<string, string> = {
  '.md': 'markdown',
  '.json': 'json',
  '.yaml': 'yaml',
  '.yml': 'yaml',
  '.toml': 'toml',
  '.sh': 'shell',
  '.bash': 'shell',
  '.zsh': 'shell',
  '.go': 'go',
  '.ts': 'typescript',
  '.tsx': 'typescript',
  '.js': 'javascript',
  '.jsx': 'javascript',
  '.mjs': 'javascript',
  '.cjs': 'javascript',
  '.py': 'python',
  '.rs': 'rust',
  '.html': 'html',
  '.htm': 'html',
  '.css': 'css',
  '.scss': 'css',
};

/** Determine the language mode from a file path. */
export function getLanguageFromPath(filePath: string): string {
  const ext = filePath.includes('.') ? '.' + filePath.split('.').pop()!.toLowerCase() : '';
  return EXTENSION_LANGUAGE_MAP[ext] || 'plaintext';
}

// ────────────────────────────────────────────────────────────
// Lazy-loaded CodeMirror setup
// ────────────────────────────────────────────────────────────

/** The CodeMirror modules loadCodeMirror resolves to. Types only. */
interface CMModules {
  view: typeof CMView;
  state: typeof CMState;
  commands: typeof CMCommands;
  language: typeof CMLanguage;
  search: typeof CMSearch;
  autocomplete: typeof CMAutocomplete;
}

let cmPromise: Promise<CMModules> | null = null;

async function loadCodeMirror(): Promise<CMModules> {
  if (!cmPromise) {
    cmPromise = (async () => {
      const [view, state, commands, language, search, autocomplete] = await Promise.all([
        import('@codemirror/view'),
        import('@codemirror/state'),
        import('@codemirror/commands'),
        import('@codemirror/language'),
        import('@codemirror/search'),
        import('@codemirror/autocomplete'),
      ]);
      return { view, state, commands, language, search, autocomplete };
    })();
  }
  return cmPromise;
}

/**
 * Language support per language id (the ids getLanguageFromPath returns).
 * Each entry is a dynamic import, so every language is its own lazily loaded
 * chunk (lang-<name>, see vite.config.ts) and opening a file fetches only
 * the language it needs.
 */
const LANGUAGE_LOADERS: Record<string, () => Promise<Extension>> = {
  javascript: async () => (await import('@codemirror/lang-javascript')).javascript({ jsx: true }),
  typescript: async () =>
    (await import('@codemirror/lang-javascript')).javascript({ jsx: true, typescript: true }),
  json: async () => (await import('@codemirror/lang-json')).json(),
  markdown: async () => (await import('@codemirror/lang-markdown')).markdown(),
  yaml: async () => (await import('@codemirror/lang-yaml')).yaml(),
  go: async () => (await import('@codemirror/lang-go')).go(),
  python: async () => (await import('@codemirror/lang-python')).python(),
  rust: async () => (await import('@codemirror/lang-rust')).rust(),
  html: async () => (await import('@codemirror/lang-html')).html(),
  css: async () => (await import('@codemirror/lang-css')).css(),
};

/**
 * The CodeMirror extension for a language id, matched case-insensitively
 * ('Go' and 'go' are the same). A language without support (plaintext,
 * shell, toml, ...) resolves to no extension without importing anything; a
 * failed import also resolves to no extension (plain text), so the editor
 * still works. Never rejects.
 */
export async function languageFor(language: string): Promise<Extension> {
  const id = language.toLowerCase();
  // Own keys only, so an id such as 'constructor' is not looked up on
  // Object.prototype.
  if (!Object.hasOwn(LANGUAGE_LOADERS, id)) return [];
  const load = LANGUAGE_LOADERS[id];
  try {
    return await load();
  } catch (err) {
    console.warn(`Failed to load ${language} syntax highlighting:`, err);
    return [];
  }
}

// ────────────────────────────────────────────────────────────
// Component
// ────────────────────────────────────────────────────────────

/** The part of a CodeMirror EditorView the selection methods use. */
interface SelectionView {
  state: {
    selection: { main: { from: number; to: number } };
    sliceDoc(from: number, to: number): string;
  };
  dispatch(spec: {
    changes: { from: number; to: number; insert: string };
    selection: { anchor: number; head: number };
    scrollIntoView: boolean;
  }): void;
  focus(): void;
}

@customElement('scion-code-editor')
export class ScionCodeEditor extends LitElement {
  /** Initial content to load into the editor. */
  @property({ type: String })
  content = '';

  /** Language mode for syntax highlighting (e.g. 'markdown', 'json'). */
  @property({ type: String })
  language = 'plaintext';

  /** Whether the editor is read-only. */
  @property({ type: Boolean })
  readonly = false;

  @state() private loading = true;
  @state() private error: string | null = null;

  private editorView: EditorView | null = null;
  private contentInitialized = false;
  /** Holds the language extension, reconfigured once the language loads. */
  private languageCompartment: Compartment | null = null;
  /** Bumped per language request; only the latest request may apply. */
  private languageRequest = 0;

  static override styles = css`
    :host {
      display: block;
      position: relative;
    }

    .editor-container {
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      overflow: hidden;
      min-height: 200px;
    }

    .editor-container .cm-editor {
      height: 100%;
      min-height: 200px;
      max-height: calc(var(--scion-app-height, 100dvh) - 16rem);
      font-size: 0.875rem;
      font-family: var(
        --scion-font-mono,
        'SF Mono',
        'Fira Code',
        'Fira Mono',
        Menlo,
        Consolas,
        monospace
      );
    }

    .editor-container .cm-editor.cm-focused {
      outline: none;
    }

    .editor-container .cm-scroller {
      overflow: auto;
    }

    .loading-state {
      display: flex;
      align-items: center;
      justify-content: center;
      padding: 3rem;
      color: var(--scion-text-muted, #64748b);
    }

    .loading-state sl-spinner {
      font-size: 1.5rem;
      margin-right: 0.75rem;
    }

    .error-state {
      padding: 1rem;
      color: var(--sl-color-danger-600, #dc2626);
      font-size: 0.875rem;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    void this.updateComplete.then(() => {
      void this.initEditor();
    });
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    this.languageRequest++;
    if (this.editorView) {
      this.editorView.destroy();
      this.editorView = null;
    }
  }

  override updated(changed: Map<string, unknown>): void {
    // If content changes externally after initialization (e.g. file reload),
    // replace the editor content.
    if (changed.has('content') && this.editorView && this.contentInitialized) {
      const currentContent = this.editorView.state.doc.toString();
      if (currentContent !== this.content) {
        this.editorView.dispatch({
          changes: { from: 0, to: currentContent.length, insert: this.content },
        });
      }
    }
    if (changed.has('language') && !changed.has('readonly') && this.editorView) {
      void this.applyLanguage(languageFor(this.language));
    }
    if (changed.has('readonly') && this.editorView) {
      this.editorView.dispatch({
        effects: (this.editorView.state.facet as unknown) ? [] : [], // readOnly is set via reconfigure
      });
      // Rebuild the editor if readonly changes — simpler than dynamic reconfiguration
      void this.initEditor();
    }
  }

  /**
   * The current selection as offsets into the content, with its text, or
   * null before the editor has loaded.
   */
  getSelection(): { from: number; to: number; text: string } | null {
    const view = this.editorView as SelectionView | null;
    if (!view) return null;
    const { from, to } = view.state.selection.main;
    return { from, to, text: view.state.sliceDoc(from, to) };
  }

  /**
   * Replaces the content between from and to with insert, then selects
   * [selectFrom, selectTo) of the result (offsets into the new content)
   * and focuses the editor. A content-changed event follows as for typing.
   */
  replaceRange(
    from: number,
    to: number,
    insert: string,
    selectFrom: number,
    selectTo: number
  ): void {
    const view = this.editorView as SelectionView | null;
    if (!view) return;
    view.dispatch({
      changes: { from, to, insert },
      selection: { anchor: selectFrom, head: selectTo },
      scrollIntoView: true,
    });
    view.focus();
  }

  /** Get the current editor content. */
  getContent(): string {
    return this.editorView?.state.doc.toString() ?? this.content;
  }

  /**
   * Puts a requested language extension into the editor once it has loaded.
   * Only the latest request applies: a slower earlier import (the language
   * changed meanwhile, or the editor was rebuilt or removed) is dropped.
   */
  private async applyLanguage(extension: Promise<Extension>): Promise<void> {
    const request = ++this.languageRequest;
    const view = this.editorView;
    const ext = await extension;
    if (request !== this.languageRequest || !view || view !== this.editorView) return;
    if (!this.languageCompartment) return;
    view.dispatch({ effects: this.languageCompartment.reconfigure(ext) });
  }

  private async initEditor(): Promise<void> {
    this.loading = true;
    this.error = null;
    // Start the language chunk download alongside the editor core; the
    // editor shows plain text until it arrives.
    const language = this.language;
    const languageReady = languageFor(language);

    try {
      const cm = await loadCodeMirror();
      const {
        EditorView,
        keymap,
        lineNumbers,
        highlightActiveLine,
        highlightActiveLineGutter,
        drawSelection,
        rectangularSelection,
        highlightSpecialChars,
        dropCursor,
      } = cm.view;
      const { EditorState, Compartment } = cm.state;
      const { defaultKeymap, history, historyKeymap, indentWithTab } = cm.commands;
      const {
        syntaxHighlighting,
        defaultHighlightStyle,
        indentOnInput,
        bracketMatching,
        foldGutter,
        foldKeymap,
      } = cm.language;
      const { searchKeymap, highlightSelectionMatches } = cm.search;
      const { autocompletion, completionKeymap, closeBrackets, closeBracketsKeymap } =
        cm.autocomplete;

      // Detect dark mode from document theme or system preference
      const isDark =
        document.documentElement.getAttribute('data-theme') === 'dark' ||
        document.documentElement.classList.contains('sl-theme-dark') ||
        document.documentElement.classList.contains('dark') ||
        (!document.documentElement.getAttribute('data-theme') &&
          window.matchMedia('(prefers-color-scheme: dark)').matches);

      // Build a dark-mode-aware highlight style
      const { HighlightStyle } = cm.language;
      const { tags } = await import('@lezer/highlight');

      const darkHighlightStyle = HighlightStyle.define([
        { tag: tags.keyword, color: '#c678dd' },
        { tag: [tags.name, tags.deleted, tags.character, tags.macroName], color: '#e06c75' },
        { tag: [tags.propertyName], color: '#61afef' },
        { tag: [tags.function(tags.variableName), tags.labelName], color: '#61afef' },
        { tag: [tags.color, tags.constant(tags.name), tags.standard(tags.name)], color: '#d19a66' },
        { tag: [tags.definition(tags.name), tags.separator], color: '#abb2bf' },
        {
          tag: [
            tags.typeName,
            tags.className,
            tags.number,
            tags.changed,
            tags.annotation,
            tags.modifier,
            tags.self,
            tags.namespace,
          ],
          color: '#e5c07b',
        },
        {
          tag: [
            tags.operator,
            tags.operatorKeyword,
            tags.url,
            tags.escape,
            tags.regexp,
            tags.special(tags.string),
          ],
          color: '#56b6c2',
        },
        { tag: [tags.meta, tags.comment], color: '#7f848e' },
        { tag: tags.strong, fontWeight: 'bold' },
        { tag: tags.emphasis, fontStyle: 'italic' },
        { tag: tags.strikethrough, textDecoration: 'line-through' },
        { tag: tags.link, color: '#61afef', textDecoration: 'underline' },
        { tag: tags.heading, fontWeight: 'bold', color: '#e06c75' },
        { tag: [tags.atom, tags.bool, tags.special(tags.variableName)], color: '#d19a66' },
        { tag: [tags.processingInstruction, tags.string, tags.inserted], color: '#98c379' },
        { tag: tags.invalid, color: '#ffffff', backgroundColor: '#e06c75' },
      ]);

      // Load language support
      const extensions = [
        lineNumbers(),
        highlightActiveLineGutter(),
        highlightSpecialChars(),
        history(),
        foldGutter(),
        drawSelection(),
        dropCursor(),
        EditorState.allowMultipleSelections.of(true),
        indentOnInput(),
        syntaxHighlighting(isDark ? darkHighlightStyle : defaultHighlightStyle, { fallback: true }),
        bracketMatching(),
        closeBrackets(),
        autocompletion(),
        rectangularSelection(),
        highlightActiveLine(),
        highlightSelectionMatches(),
        keymap.of([
          ...closeBracketsKeymap,
          ...defaultKeymap,
          ...searchKeymap,
          ...historyKeymap,
          ...foldKeymap,
          ...completionKeymap,
          indentWithTab,
        ]),
        EditorView.lineWrapping,
        // Theme: match our design system colors
        EditorView.theme({
          '&': {
            backgroundColor: 'var(--scion-surface, #ffffff)',
            color: 'var(--scion-text, #1e293b)',
          },
          '.cm-gutters': {
            backgroundColor: 'var(--scion-bg-subtle, #f8fafc)',
            color: 'var(--scion-text-muted, #64748b)',
            borderRight: '1px solid var(--scion-border, #e2e8f0)',
          },
          '.cm-activeLineGutter': {
            backgroundColor: isDark
              ? 'rgba(255, 255, 255, 0.05)'
              : 'var(--sl-color-primary-50, #eff6ff)',
          },
          '.cm-activeLine': {
            backgroundColor: isDark
              ? 'rgba(255, 255, 255, 0.05)'
              : 'var(--sl-color-primary-50, #eff6ff)',
          },
          '&.cm-focused .cm-cursor': {
            borderLeftColor: isDark
              ? 'var(--sl-color-primary-400, #60a5fa)'
              : 'var(--sl-color-primary-600, #2563eb)',
          },
          '&.cm-focused .cm-selectionBackground, ::selection': {
            backgroundColor: isDark
              ? 'rgba(97, 175, 239, 0.25)'
              : 'var(--sl-color-primary-100, #dbeafe)',
          },
          '.cm-selectionMatch': {
            backgroundColor: isDark
              ? 'rgba(229, 192, 123, 0.2)'
              : 'var(--sl-color-warning-100, #fef3c7)',
          },
          '.cm-matchingBracket': isDark
            ? { backgroundColor: 'rgba(97, 175, 239, 0.3)', color: '#ffffff !important' }
            : {},
        }),
      ];

      // Language support arrives through the compartment once loaded.
      const languageCompartment = new Compartment();
      extensions.push(languageCompartment.of([]));

      // Read-only mode
      if (this.readonly) {
        extensions.push(EditorState.readOnly.of(true));
        extensions.push(EditorView.editable.of(false));
      }

      // Change listener: dispatch content-changed event
      extensions.push(
        EditorView.updateListener.of(
          (update: { docChanged: boolean; state: { doc: { toString(): string } } }) => {
            if (update.docChanged) {
              this.dispatchEvent(
                new CustomEvent('content-changed', {
                  detail: { content: update.state.doc.toString() },
                  bubbles: true,
                  composed: true,
                })
              );
            }
          }
        )
      );

      // Destroy previous editor if exists
      if (this.editorView) {
        this.editorView.destroy();
      }

      const container = this.shadowRoot?.querySelector('.editor-mount');
      if (!container) return;

      // Clear mount point
      container.replaceChildren();

      this.editorView = new EditorView({
        state: EditorState.create({
          doc: this.content,
          extensions,
        }),
        parent: container,
      });
      this.languageCompartment = languageCompartment;

      this.contentInitialized = true;
      void this.applyLanguage(
        language === this.language ? languageReady : languageFor(this.language)
      );
    } catch (err) {
      console.error('Failed to initialize code editor:', err);
      this.error = 'Failed to load editor';
    } finally {
      this.loading = false;
    }
  }

  override render() {
    if (this.error) {
      return html`<div class="error-state">${this.error}</div>`;
    }

    return html`
      <div class="editor-container">
        ${this.loading
          ? html`
              <div class="loading-state">
                <sl-spinner></sl-spinner>
                Loading editor...
              </div>
            `
          : ''}
        <div class="editor-mount" style="${this.loading ? 'display:none' : ''}"></div>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-code-editor': ScionCodeEditor;
  }
}
