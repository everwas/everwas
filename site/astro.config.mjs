// @ts-check
import { defineConfig } from 'astro/config';
import sitemap from '@astrojs/sitemap';
import icon from 'astro-icon';

// The public domain is swappable via SITE_URL (see .env / docker-compose).
// Telemetry is disabled via ASTRO_TELEMETRY_DISABLED=1 in the Dockerfile.
const site = process.env.SITE_URL ?? 'https://everwas.supported.systems';

export default defineConfig({
  site,
  integrations: [sitemap(), icon()],
  devToolbar: { enabled: false },
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
