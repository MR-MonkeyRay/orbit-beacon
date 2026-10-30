FROM --platform=$BUILDPLATFORM golang:1.27.3-bookworm@sha256:d9b6a1beb9474adc2a1ce842573639b44b2a0dd49b6669f51d3821baf9ec37c9 AS build

ARG VERSION=development
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN install -d -m 0755 /out
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -trimpath \
    -ldflags "-s -w -X orbitbeacon/internal/buildinfo.Version=${VERSION} -X orbitbeacon/internal/buildinfo.Commit=${COMMIT} -X orbitbeacon/internal/buildinfo.BuildDate=${BUILD_DATE}" \
    -o /out/orbitbeacon-server ./cmd/orbitbeacon-server
RUN install -d -m 0700 -o 65532 -g 65532 /out/data

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
ARG VERSION=development
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
LABEL org.opencontainers.image.title="OrbitBeacon" \
    org.opencontainers.image.description="Self-hosted multi-node monitoring server with an embedded web UI" \
    org.opencontainers.image.source="https://github.com/mr-monkeyray/orbit-beacon" \
    org.opencontainers.image.licenses="GPL-3.0-only" \
    org.opencontainers.image.version="${VERSION}" \
    org.opencontainers.image.revision="${COMMIT}" \
    org.opencontainers.image.created="${BUILD_DATE}"
COPY --from=build /out/orbitbeacon-server /usr/local/bin/orbitbeacon-server
COPY --from=build --chown=nonroot:nonroot --chmod=0700 /out/data /var/lib/orbitbeacon
COPY deploy/docker/server.json /etc/orbitbeacon/server.json
EXPOSE 8080
USER nonroot:nonroot
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 CMD ["/usr/local/bin/orbitbeacon-server","health","--url","http://127.0.0.1:8080"]
ENTRYPOINT ["/usr/local/bin/orbitbeacon-server", "serve", "--config", "/etc/orbitbeacon/server.json"]
