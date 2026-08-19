# Everwas marketing site

Static Astro site for everwas.supported.systems (domain swappable via
`DOMAIN` in `.env` — nothing else changes).

## Local development

```sh
npm install
npm run dev        # http://localhost:4321
npm run build      # static build into dist/
```

Or containerized, behind caddy-docker-proxy:

```sh
cp .env.example .env
make dev           # Astro dev server with HMR
make prod          # Caddy serving the built dist/
```

## Layout

- `src/pages/index.astro` — the single landing page, assembled from components
- `src/components/` — one `.astro` + one `.css` per section
- `src/styles/tokens.css` — design tokens. Amber marks valid time (true on
  the machine), cyan marks record time (known to the server); every section
  that touches the bitemporal story reuses that pairing.
- `Dockerfile` / `Caddyfile` / `docker-compose.yml` / `Makefile` — the
  standard static-site deploy: Node builder stage, Caddy prod stage,
  `dev` compose profile for HMR. The Caddyfile serves missing paths with a
  real HTTP 404 status.
