/**
 * Isolated fixture for the extracted `<scion-chat-file-preview>` (recent
 * files and extracted viewer). Mounts the real `<scion-chat-thread>`
 * directly — its real `<scion-chat-message>` children exercise the
 * attachment-expansion overlay, and the thread itself exercises the
 * path-link overlay — with endpoint-shaped request interception, no live Hub.
 *
 * Both surfaces delegate to the same reusable, separately-shadow-rooted
 * `<scion-chat-file-preview>`; this is a nested-shadow-DOM composedPath/event
 * scenario that needs real Chromium, not happy-dom, to prove.
 */
import '../../src/components/shared/chat/chat-thread.js';
import type { ScionChatThread } from '../../src/components/shared/chat/chat-thread.js';
import '@shoelace-style/shoelace/dist/components/button/button.js';
import '@shoelace-style/shoelace/dist/components/icon/icon.js';
import '@shoelace-style/shoelace/dist/components/icon-button/icon-button.js';
import '@shoelace-style/shoelace/dist/components/spinner/spinner.js';
import '@shoelace-style/shoelace/dist/components/textarea/textarea.js';
import '@shoelace-style/shoelace/dist/components/tooltip/tooltip.js';
import '@shoelace-style/shoelace/dist/components/dialog/dialog.js';
import '@shoelace-style/shoelace/dist/components/dropdown/dropdown.js';
import '@shoelace-style/shoelace/dist/components/menu/menu.js';
import '@shoelace-style/shoelace/dist/components/menu-item/menu-item.js';
import '@shoelace-style/shoelace/dist/themes/light.css';
import { CONVERSATION_KEY, SELF_USER_ID } from './data.js';

const thread = document.createElement('scion-chat-thread') as ScionChatThread;
thread.conversationKey = CONVERSATION_KEY;
thread.isDM = true;
thread.currentUserId = SELF_USER_ID;
thread.canSend = true;

document.getElementById('thread-outlet')!.appendChild(thread);

declare global {
  interface Window {
    chatFilePreviewFixture: { thread: ScionChatThread };
  }
}
window.chatFilePreviewFixture = { thread };
