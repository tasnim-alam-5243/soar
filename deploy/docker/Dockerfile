# syntax=docker/dockerfile:1
# One image for every Go role; the api role also serves the web UI (embedded at build time).

# 1. the React console (web/) -> internal/webui/dist
FROM node:22-bookworm-slim AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY api/openapi.yaml /src/api/openapi.yaml
COPY web/ ./
RUN npm run gen:api && npm run build

# 2. the soar binary with the UI bundle embedded (-tags ui)
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
COPY --from=web /src/internal/webui/dist /src/internal/webui/dist
ARG VERSION=dev
ARG COMMIT=none
RUN CGO_ENABLED=0 go build -tags ui -trimpath \
      -ldflags "-s -w -X github.com/yourorg/soar/internal/obs.Version=${VERSION} -X github.com/yourorg/soar/internal/obs.Commit=${COMMIT}" \
      -o /out/soar ./cmd/soar

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/soar /soar
COPY --from=build /src/config/retention.yaml /etc/soar/retention.yaml
USER nonroot:nonroot
ENTRYPOINT ["/soar"]
