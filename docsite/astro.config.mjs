// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import mermaid from 'astro-mermaid';

// The public domain is swappable via SITE_URL (see .env / docker-compose).
// Telemetry is disabled via ASTRO_TELEMETRY_DISABLED=1 in the Dockerfile.
const site = process.env.SITE_URL ?? 'https://docs.everwas.supported.systems';

export default defineConfig({
  site,
  devToolbar: { enabled: false },
  integrations: [
    // Client-side mermaid with theme switching; must precede starlight so
    // it claims the ```mermaid fences before syntax highlighting does.
    mermaid({
      theme: 'base',
      autoTheme: true,
      mermaidConfig: {
        // The site's two load-bearing hues: amber (valid time) and cyan
        // (record time). Diagrams inherit the family look.
        themeVariables: {
          primaryColor: '#1d2230',
          primaryTextColor: '#e9e7e1',
          primaryBorderColor: '#ffb454',
          lineColor: '#5fc9e8',
          secondaryColor: '#161a23',
          tertiaryColor: '#10131a',
          fontFamily: 'Inter Variable, system-ui, sans-serif',
        },
      },
    }),
    starlight({
      title: 'Everwas',
      description:
        'Documentation for Everwas: open-source remote monitoring and management with bitemporal device history and a first-class MCP server.',
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
            'guides/network-authentication',
            'guides/alerts',
            'guides/device-history',
            'guides/sync-to-nautobot',
            'guides/enable-mcp',
          ],
        },
        {
          label: 'Reference',
          items: [
            'reference/cli',
            'reference/environment',
            'reference/mcp-tools',
            'reference/posture-checks',
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
            'concepts/security-posture',
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
