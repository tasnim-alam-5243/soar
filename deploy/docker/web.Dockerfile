# syntax=docker/dockerfile:1
# Web UI (spec 12). Build context: repository root.
FROM node:22-alpine AS build
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY api/openapi.yaml /src/api/openapi.yaml
COPY web/ ./
RUN npm run gen:api && npm run build

FROM nginxinc/nginx-unprivileged:1.27-alpine
# Rendered at start into /tmp (the root filesystem is read-only).
ENV NGINX_ENVSUBST_OUTPUT_DIR=/tmp/nginx \
    SOAR_API_UPSTREAM=http://api:8080 \
    SOAR_OIDC_AUTHORITY=http://localhost:8180/realms/soar \
    SOAR_OIDC_CLIENT_ID=soar-web \
    SOAR_OIDC_ORIGIN=http://localhost:8180
COPY deploy/web/nginx.conf /etc/nginx/nginx.conf
COPY deploy/web/default.conf.template /etc/nginx/templates/default.conf.template
COPY --chmod=755 deploy/web/12-soar-config.sh /docker-entrypoint.d/12-soar-config.sh
COPY --from=build /src/web/dist /usr/share/nginx/html
EXPOSE 8080
