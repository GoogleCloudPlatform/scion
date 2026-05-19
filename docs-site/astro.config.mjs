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

// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import d2 from 'astro-d2';
import starlightLinksValidator from 'starlight-links-validator';

// https://astro.build/config
export default defineConfig({
	site: 'https://googlecloudplatform.github.io',
	base: '/scion',
	integrations: [
		d2(),
		starlight({
			plugins: [starlightLinksValidator()],
			title: 'Scion',
			social: [
				{ icon: 'github', label: 'GitHub', href: 'https://github.com/GoogleCloudPlatform/scion' },
			],
			sidebar: [
				{
					label: 'Introduction & Foundations',
					items: [
						{ label: 'Overview', slug: 'overview' },
						{ label: 'Core Concepts', slug: 'concepts' },
						{ label: 'Philosophy', slug: 'philosophy' },
						{ label: 'Supported Harnesses', slug: 'supported-harnesses' },
						{ label: 'Glossary', slug: 'glossary' },
						{ label: 'Release Notes', slug: 'release-notes' },
					],
				},
				{
					label: 'Getting Started',
					items: [
						{ label: 'Installation', slug: 'getting-started/install' },
						{ label: 'Tutorial', slug: 'getting-started/tutorial' },
					],
				},
				{
					label: 'Advanced Local Usage',
					items: [
						{ label: 'Local Configuration', slug: 'advanced-local/local-governance' },
						{ label: 'Templates & Roles', slug: 'advanced-local/templates' },
						{ label: 'Custom Images', slug: 'advanced-local/custom-images' },
						{ label: 'Agent Credentials', slug: 'advanced-local/agent-credentials' },
						{ label: 'About Workspaces', slug: 'advanced-local/workspace' },
						{ label: 'Tmux Sessions', slug: 'advanced-local/tmux' },
						{ label: 'Shell Completions', slug: 'advanced-local/completions' },
						{ label: 'Workstation Server', slug: 'advanced-local/workstation-server' },
					],
				},
				{
				        label: 'Hub User Guide',
				        items: [
				                { label: 'Connecting to Hub', slug: 'hub-user/hosted-user' },
				                { label: 'Personal Access Tokens', slug: 'hub-user/personal-access-tokens' },
				                { label: 'Git-Based Projects', slug: 'hub-user/git-projects' },
				                { label: 'Web Dashboard', slug: 'hub-user/dashboard' },
				                { label: 'Secret Management', slug: 'hub-user/secrets' },
				                { label: 'Runtime Broker', slug: 'hub-user/runtime-broker' },
				                { label: 'Messaging & Notifications', slug: 'hub-user/messaging' },
				        ],
				},
				{
				        label: 'Hub Administration',					items: [
						{ label: 'Hub Setup', slug: 'hub-admin/hub-server' },
						{ label: 'Kubernetes', slug: 'hub-admin/kubernetes' },
						{ label: 'Security', slug: 'hub-admin/auth' },
						{ label: 'Permissions', slug: 'hub-admin/permissions' },
						{ label: 'Observability', slug: 'hub-admin/observability' },
						{ label: 'Metrics', slug: 'hub-admin/metrics' },
					],
				},
				{
					label: 'Technical Reference',
					autogenerate: { directory: 'reference' },
				},
				{
					label: 'Development',
					items: [
						{ label: 'Local Logging', slug: 'development/logging' },
					],
				},
				{
					label: 'Contributing',
					autogenerate: { directory: 'contributing' },
				},
			],
		}),
	],
});
