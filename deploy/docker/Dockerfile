# syntax=docker/dockerfile:1
FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/yourorg/soar/internal/obs.Version=${VERSION} -X github.com/yourorg/soar/internal/obs.Commit=${COMMIT}" \
      -o /out/soar ./cmd/soar

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/soar /soar
COPY --from=build /src/config/retention.yaml /etc/soar/retention.yaml
USER nonroot:nonroot
ENTRYPOINT ["/soar"]
