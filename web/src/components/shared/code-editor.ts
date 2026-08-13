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

// eslint-disable-next-line @typescript-eslint/no-explicit-any
type CMModule = any;

let cmPromise: Promise<CMModule> | null = null;

async function loadCodeMirror(): Promise<CMModule> {
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

async function loadLanguageSupport(lang: string): Promise<CMModule | null> {
  switch (lang) {
    case 'javascript':
    case 'typescript':
      return import('@codemirror/lang-javascript');
    case 'json':
      return import('@codemirror/lang-json');
    case 'markdown':
      return import('@codemirror/lang-markdown');
    case 'yaml':
      return import('@codemirror/lang-yaml');
    case 'go':
      return import('@codemirror/lang-go');
    case 'python':
      return import('@codemirror/lang-python');
    case 'rust':
      return import('@codemirror/lang-rust');
    case 'html':
      return import('@codemirror/lang-html');
    case 'css':
      return import('@codemirror/lang-css');
    default:
      return null;
  }
}

// ────────────────────────────────────────────────────────────
// Component
// ────────────────────────────────────────────────────────────

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

  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  private editorView: any = null;
  private contentInitialized = false;

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
      max-height: calc(100vh - 16rem);
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

  override async connectedCallback(): Promise<void> {
    super.connectedCallback();
    await this.updateComplete;
    void this.initEditor();
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
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
    if (changed.has('readonly') && this.editorView) {
      this.editorView.dispatch({
        effects: this.editorView.state.facet ? [] : [], // readOnly is set via reconfigure
      });
      // Rebuild the editor if readonly changes — simpler than dynamic reconfiguration
      void this.initEditor();
    }
  }

  /** Get the current editor content. */
  getContent(): string {
    return this.editorView?.state.doc.toString() ?? this.content;
  }

  private async initEditor(): Promise<void> {
    this.loading = true;
    this.error = null;

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
      const { EditorState } = cm.state;
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

      // Add language support if available
      const langMod = await loadLanguageSupport(this.language);
      if (langMod) {
        if (this.language === 'javascript') {
          extensions.push(langMod.javascript({ jsx: true }));
        } else if (this.language === 'typescript') {
          extensions.push(langMod.javascript({ jsx: true, typescript: true }));
        } else {
          // Most language modules export a function named after the language
          const langFn = langMod[this.language] || langMod.default;
          if (typeof langFn === 'function') {
            extensions.push(langFn());
          }
        }
      }

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
      container.innerHTML = '';

      this.editorView = new EditorView({
        state: EditorState.create({
          doc: this.content,
          extensions,
        }),
        parent: container,
      });

      this.contentInitialized = true;
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
