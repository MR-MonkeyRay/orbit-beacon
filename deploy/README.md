# Deployment materials

These files provide the smallest supported process/container boundaries. They do not provision TLS or a reverse proxy; `public_url` must be the HTTPS browser origin served by the operator's proxy or by a separately configured TLS listener.

## systemd

1. Create the service account and install the binaries/configuration:

```sh
sudo useradd --system --user-group --home /var/lib/orbitbeacon --shell /usr/sbin/nologin orbitbeacon
sudo install -D -o root -g root -m 0755 bin/orbitbeacon-server /usr/local/bin/orbitbeacon-server
sudo install -D -o root -g orbitbeacon -m 0640 deploy/server.systemd.json /etc/orbitbeacon/server.json
sudo install -D -o root -g root -m 0644 deploy/orbitbeacon-server.service /etc/systemd/system/orbitbeacon-server.service
```

2. Set `data_dir` to `/var/lib/orbitbeacon`, set the real `public_url`, and configure the reverse proxy for `127.0.0.1:8080`. Then enable the unit:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now orbitbeacon-server
curl --fail https://beacon.example.test/healthz/ready
```

`StateDirectory=orbitbeacon` creates the private state directory with mode 0700. The unit uses a non-login service account, `ProtectSystem=strict`, `PrivateTmp`, `NoNewPrivileges`, restricted address families and explicit writable state only.

## Container

Build and run through Compose after reviewing `server.container.json` and replacing its public URL:

```sh
docker compose -f deploy/compose.yaml build \
  --build-arg VERSION=1.2.3 \
  --build-arg COMMIT="$(git rev-parse HEAD)" \
  --build-arg BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
docker compose -f deploy/compose.yaml up -d
```

The image runs as distroless `nonroot`, exposes only port 8080, keeps the root filesystem read-only, drops Linux capabilities, and persists only `/var/lib/orbitbeacon`. The image seeds that directory as UID/GID 65532 with mode 0700; the named volume must remain owned and writable by UID/GID 65532 with mode 0700. Do not replace it with a host bind mount unless those ownership and ancestry constraints are preserved.
