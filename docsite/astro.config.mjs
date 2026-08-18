// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';

// The public domain is swappable via SITE_URL (see .env / docker-compose).
// Telemetry is disabled via ASTRO_TELEMETRY_DISABLED=1 in the Dockerfile.
const site = process.env.SITE_URL ?? 'https://docs.openrmm.supported.systems';

export default defineConfig({
  site,
  devToolbar: { enabled: false },
  integrations: [
    starlight({
      title: 'OpenRMM',
      description:
        'Documentation for OpenRMM: open-source remote monitoring and management with bitemporal device history and a first-class MCP server.',
      favicon: '/favicon.svg',
      customCss: [
        '@fontsource-variable/inter',
        '@fontsource-variable/space-grotesk',
        '@fontsource-variable/jetbrains-mono',
        './src/styles/custom.css',
      ],
      sidebar: [
        {
          label: 'Start here',
          items: [
            'getting-started/quickstart',
            'getting-started/enroll-an-agent',
          ],
        },
        {
          label: 'Guides',
          items: [
            'guides/run-scripts',
            'guides/schedules',
            'guides/patch-management',
            'guides/certificates',
            'guides/alerts',
            'guides/device-history',
            'guides/enable-mcp',
          ],
        },
        {
          label: 'Reference',
          items: [
            'reference/cli',
            'reference/environment',
            'reference/mcp-tools',
            'reference/sync-api',
            'reference/wire-protocol',
          ],
        },
        {
          label: 'Concepts',
          items: [
            'concepts/architecture',
            'concepts/bitemporal',
            'concepts/security-model',
            'concepts/licensing',
          ],
        },
        {
          label: 'Decisions',
          items: [{ autogenerate: { directory: 'decisions' } }],
        },
      ],
    }),
  ],
  vite: {
    server: {
      host: '0.0.0.0',
      // HMR behind the TLS-terminating caddy-docker-proxy front-end.
      hmr: process.env.DOMAIN
        ? { host: process.env.DOMAIN, protocol: 'wss', clientPort: 443 }
        : undefined,
    },
  },
});
