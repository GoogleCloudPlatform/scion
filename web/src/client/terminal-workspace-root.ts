import { TerminalSessionRegistry, type TerminalSession } from './terminal-sessions.js';
import type { ScionTerminalPane } from '../components/terminal/terminal-pane.js';
import '../components/terminal/terminal-pane.js';

/** Document-lived presentation. Pane nodes never move through disposable shells. */
export class TerminalWorkspaceRoot {
  readonly element = document.createElement('div');
  private readonly message = document.createElement('p');
  private readonly panes = new Map<string, ScionTerminalPane>();
  private selected: string | null = null;

  constructor() {
    this.element.id = 'terminal-workspace';
    this.element.hidden = true;
    this.element.style.cssText = 'height:100vh;min-height:0;display:none';
    this.message.textContent = 'No terminal selected.';
    this.message.style.cssText = 'padding:1rem';
    this.element.appendChild(this.message);
  }

  create(registry: TerminalSessionRegistry, agentId: string): TerminalSession {
    const pane = document.createElement('scion-terminal-pane');
    pane.style.cssText = 'height:100%;width:100%;min-height:0';
    pane.setVisible(false);
    this.element.appendChild(pane);
    try {
      const session = pane.open(registry, agentId);
      this.panes.set(session.state.key, pane);
      const unsubscribe = session.subscribe((state) => {
        if (state.connection !== 'closed') return;
        unsubscribe();
        if (this.panes.get(state.key) === pane) this.panes.delete(state.key);
        if (this.selected === state.key) this.selected = null;
        pane.remove();
      });
      return session;
    } catch (error) {
      pane.remove();
      throw error;
    }
  }

  select(session: TerminalSession): void {
    const pane = this.panes.get(session.state.key);
    if (!pane) throw new Error('Terminal session has no retained pane.');
    this.selected = session.state.key;
    this.message.hidden = true;
    this.show(true);
  }

  setStatus(message: string): void {
    if (this.selected) return;
    this.message.textContent = message;
    this.message.hidden = false;
  }

  show(visible: boolean): void {
    this.element.hidden = !visible;
    this.element.style.display = visible ? 'block' : 'none';
    for (const [key, pane] of this.panes) pane.setVisible(visible && key === this.selected);
  }
}
