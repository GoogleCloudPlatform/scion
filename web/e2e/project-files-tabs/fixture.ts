// Isolated production project-detail fixture: the hub API is supplied by
// Playwright route mocks (see mock-api.ts), not a real backend.

import '@shoelace-style/shoelace/dist/themes/light.css';
import { setBasePath } from '@shoelace-style/shoelace/dist/utilities/base-path.js';
setBasePath('/shoelace');

// Every Shoelace component project-detail.ts's template can reach —
// mirrors src/client/main.ts's registration list. The autoloader cannot
// detect sl-* elements inside LitElement shadow roots, so each must be
// imported directly.
import '@shoelace-style/shoelace/dist/components/button/button.js';
import '@shoelace-style/shoelace/dist/components/checkbox/checkbox.js';
import '@shoelace-style/shoelace/dist/components/icon/icon.js';
import '@shoelace-style/shoelace/dist/components/icon-button/icon-button.js';
import '@shoelace-style/shoelace/dist/components/input/input.js';
import '@shoelace-style/shoelace/dist/components/spinner/spinner.js';
import '@shoelace-style/shoelace/dist/components/tooltip/tooltip.js';
import '@shoelace-style/shoelace/dist/components/dialog/dialog.js';
import '@shoelace-style/shoelace/dist/components/dropdown/dropdown.js';
import '@shoelace-style/shoelace/dist/components/menu/menu.js';
import '@shoelace-style/shoelace/dist/components/menu-item/menu-item.js';
import '@shoelace-style/shoelace/dist/components/alert/alert.js';
import '@shoelace-style/shoelace/dist/components/tab-group/tab-group.js';
import '@shoelace-style/shoelace/dist/components/tab/tab.js';
import '@shoelace-style/shoelace/dist/components/tab-panel/tab-panel.js';

// Registers <scion-page-project-detail> and, transitively, the file
// browser/editor/agent-tree/etc. components it imports.
import '../../src/components/pages/project-detail.js';
import { PROJECT_ID } from './mock-api.js';

import type { PageData } from '../../src/shared/types.js';

// ?spacer=1 pushes the component below the fold, so the Files section's
// IntersectionObserver-based deferral (see project-detail.ts's
// observeFilesSection()) can be exercised in a real viewport instead of
// always intersecting immediately.
if (new URLSearchParams(location.search).get('spacer')) {
  const spacer = document.createElement('div');
  spacer.style.height = '2000px';
  spacer.dataset.testSpacer = 'true';
  document.body.append(spacer);
}

const el = document.createElement('scion-page-project-detail') as HTMLElement & {
  projectId: string;
  pageData: PageData | null;
};
el.projectId = PROJECT_ID;
el.pageData = {
  path: `/projects/${PROJECT_ID}`,
  title: 'Project',
  user: { id: 'e2e-user', email: 'e2e-user@example.com', name: 'E2E User', role: 'member' },
};
document.body.append(el);
