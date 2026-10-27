FROM golang:1.27.3-bookworm AS build

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

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/orbitbeacon-server /usr/local/bin/orbitbeacon-server
COPY --from=build --chown=nonroot:nonroot --chmod=0700 /out/data /var/lib/orbitbeacon
COPY deploy/server.container.json /etc/orbitbeacon/server.json
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/orbitbeacon-server", "serve", "--config", "/etc/orbitbeacon/server.json"]
