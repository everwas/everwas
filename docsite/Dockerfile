# Multi-stage build for the OpenRMM docs site (Starlight).
#
#   docker compose --profile dev up → dev target (astro dev + HMR)
#   docker compose up (no profile)  → prod target (caddy serves dist/)

# Pinned through the mirror.gcr.io pass-through to dodge intermittent
# Docker Hub TLS hiccups during builds.
FROM mirror.gcr.io/library/node:22-alpine AS base
WORKDIR /app
COPY package.json package-lock.json ./
RUN --mount=type=cache,target=/root/.npm \
    npm ci --no-audit --no-fund


# ----- dev: astro dev server with HMR -----
FROM base AS dev
COPY . .
ENV ASTRO_TELEMETRY_DISABLED=1
EXPOSE 4321
CMD ["npm", "run", "dev"]


# ----- builder: produce dist/ -----
FROM base AS builder
COPY . .
ENV ASTRO_TELEMETRY_DISABLED=1
ARG SITE_URL
ENV SITE_URL=${SITE_URL}
RUN npm run build


# ----- prod: caddy serves the static build -----
FROM mirror.gcr.io/library/caddy:2-alpine AS prod
# caddy-docker-proxy on the host handles TLS and routing; this container
# just serves files locally.
RUN mkdir -p /srv/site
COPY --from=builder /app/dist /srv/site
COPY Caddyfile /etc/caddy/Caddyfile
EXPOSE 80
