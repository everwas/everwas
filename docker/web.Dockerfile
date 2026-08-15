FROM node:22-alpine AS deps
WORKDIR /app
COPY web/package.json web/package-lock.json ./
RUN npm ci

FROM deps AS dev
COPY web ./
CMD ["npm", "run", "dev", "--", "--host", "0.0.0.0", "--port", "5173"]

FROM deps AS build
COPY web ./
RUN npm run build

FROM caddy:2-alpine AS prod
COPY --from=build /app/dist /srv
COPY docker/web.Caddyfile /etc/caddy/Caddyfile
