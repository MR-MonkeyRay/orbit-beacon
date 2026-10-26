# OrbitBeacon

OrbitBeacon is a self-hosted node monitoring project. This delivery implements the step 1 foundation control plane: local SQLite migrations, administrator bootstrap, user/group management and secure browser sessions, persistent pending nodes with editable metadata, audit reads, bounded explicit batch actions and versioned desired configuration. It does **not** enroll agents, collect telemetry, send alerts or perform upgrades. Unavailable commands return errors rather than pretending to complete these operations.

## Build and run

Requires Go **1.27.3**. Dependencies are pinned in `go.mod` and `go.sum`.

```sh
GOTOOLCHAIN=go1.27.3 go build -o bin/orbitbeacon-server ./cmd/orbitbeacon-server
GOTOOLCHAIN=go1.27.3 go build -o bin/orbitbeacon-agent ./cmd/orbitbeacon-agent
mkdir -m 700 data
bin/orbitbeacon-server bootstrap --data-dir ./data --username admin
```

Bootstrap prompts for a password (hidden on a terminal). For automation, pass the password as one line on standard input, never a command-line flag. If `--username` is omitted, the first input line is the username. Passwords must be 8–1024 bytes. Bootstrap refuses to replace any existing administrator; there is no HTTP bootstrap endpoint.

Create a local `server.json`:

```json
{
  "listen_addr": "127.0.0.1:8080",
  "data_dir": "./data",
  "public_url": "https://beacon.example.test",
  "max_nodes": 500
}
```

```sh
bin/orbitbeacon-server serve --config server.json
```

Place an HTTPS reverse proxy in front of the loopback listener, serving the same origin as `public_url`, or configure both `tls_cert_file` and `tls_key_file` and explicitly set a TLS listen port such as `127.0.0.1:8443`. Browser session cookies are always Secure, so use HTTPS even during interactive local development. Containers must explicitly set `listen_addr` to `0.0.0.0:8080` and mount a persistent local data directory owned by the service's effective user with mode `0700`. SQLite, its WAL/SHM/journal sidecars and `secrets.key` must be regular files owned by that user with mode `0600`; symlink paths are rejected. Linux startup validates that the backing mount is local and writable. Do not store SQLite on a network or read-only filesystem.

Server keys are `listen_addr`, `data_dir`, `public_url`, `tls_cert_file`, `tls_key_file`, `trusted_proxy_cidrs`, `max_nodes`. Ordinary values use explicit CLI flags > `ORBITBEACON_` environment variables > JSON file > defaults. CLI names use hyphens; JSON/environment names use underscores (for example `--max-nodes` and `ORBITBEACON_MAX_NODES`). Proxy CIDRs in the environment or flags are comma-separated. The foundation does not consume forwarded headers; neither client Host nor forwarded headers can redefine the trusted browser Origin. Defaults are loopback port 8080, `./data`, `https://localhost` and 500 nodes. Set the real public URL explicitly; it must be absolute HTTPS with no credentials, query or fragment. Its trusted origin is canonicalized: DNS names are lowercase, default port 443 is omitted and IPv6 literals are normalized. Local paths cannot be SQLite DSNs or URIs. Capacity must be positive; pending and disabled nodes count. Increasing capacity is an admission setting, not a resource-budget guarantee.

The tracked HTML/JavaScript UI is embedded directly into the binary and needs no Node process or frontend build. It supports real login/logout, inventory, pending node creation, node metadata and desired interval changes. Settings provides own-password changes for both roles, user/group lists and the first 50 audit events; administrator controls create/update/delete users and groups. Successful password changes require a fresh login. This is not the later full monitoring UI. `/` redirects to `/login` or `/overview` based on session validity. Only `/login`, `/overview`, `/nodes`, `/nodes/{uuid}`, `/alerts`, `/settings`, `/status` receive UI fallback. Unknown API routes and assets return true 404s; assets support GET/HEAD. Alerts and status remain explicitly unavailable.

## HTTP contract and security

See [`api/openapi.yaml`](api/openapi.yaml) for the implemented request/response schemas. JSON bodies must contain one object, are limited to 64 KiB and reject unknown members. String length limits are measured in UTF-8 bytes. Errors use `{code,message,request_id,fields?}` and `X-Request-ID`. IDs are UUIDs and timestamps are UTC RFC3339. Unsupported methods return `405` with `Allow` **before authentication**, unknown API routes return structured `404`, and API errors never return the UI shell. Invalid input returns `400`, missing/expired sessions or incorrect old-password proof return `401`, authorization/Origin/CSRF failures return `403`, missing resources return `404`, and last-administrator/capacity conflicts return `409`. Implemented routes:

- `POST/DELETE /api/v1/session`, `GET /api/v1/me`. Logout is available to either role and returns `204`.
- `PATCH /api/v1/me/password`: `{old_password,new_password}`; either role may change their own password after proving the old one. The new password must be 8–1024 bytes. Success returns `204` and atomically revokes **every** session of that user, including the current one; log in again.
- `GET/POST /api/v1/users`, `GET/PATCH/DELETE /api/v1/users/{id}`. Reads return credential-free user DTOs to either authenticated role; the collection is `{users:[...]}`. Admin creation accepts `{username,password,role}`. PATCH accepts optional `username` and `role` only; omitted fields are unchanged. Usernames must be non-whitespace and 1–128 bytes, roles are `admin`/`viewer`. A role change revokes all affected-user sessions; deletion also revokes sessions. The last administrator cannot be demoted or deleted (`409 last_administrator`).
- `GET/POST /api/v1/groups`, `GET/PATCH/DELETE /api/v1/groups/{id}`. Reads are available to either authenticated role and return `{groups:[...]}` or `{id,name}`. Admin creation/rename accepts `{name}` only (non-whitespace, at most 255 bytes). Deletion atomically detaches member nodes (`group_id:null`); it does not delete those nodes.
- `GET/POST /api/v1/nodes`, `GET/PATCH/DELETE /api/v1/nodes/{id}` (delete requires `?confirm=true`). Node lists return `{nodes:[...]}`, default to 50, and accept `limit` 1–100 and nonnegative `offset`. Every node DTO includes `id,name,status,notes,region,provider,group_id,tags,created_at`; `group_id` is a UUID or `null`, and `tags` is a sorted string array, including `[]` for no tags. Admin metadata PATCH accepts optional `name,notes,region,provider,group_id,tags`; omitted fields are unchanged, `group_id:null` clears membership, and `tags:[]` clears tags. Empty `notes`, `region` or `provider` strings clear those fields; null string fields and null `tags` leave them unchanged. Tags must be unique non-whitespace strings of at most 255 bytes each and are not trimmed or normalized. Metadata edits preserve node credentials.
- `GET/PATCH /api/v1/nodes/{id}/config`. PATCH accepts report interval 5–60 seconds, disk interval 60–3600 seconds, billing day 1–28, at most eight interface names/disk paths, and unsigned 64-bit decimal `quota_bytes`. Invalid fields are returned together without changing configuration or revision. Probe targets cannot be changed until the probe slice exists.
- `POST /api/v1/nodes/batch`: `{node_ids:[...],action:"disable"|"enable"|"delete",confirm?:true}`. Select 1–500 unique UUIDs; delete requires confirmation. Each asset has its own result and transaction. Enable returns unenrolled assets to pending, not online.
- `POST /api/v1/enrollments` enforces the admin boundary but returns explicit 501; it never issues a nonfunctional token.
- `GET /api/v1/audit`: available to either authenticated role; returns `{events:[{id,actor_id,action,target_id,created_at,details:{}}]}`. `limit` is 1–100 (default 50), `offset` is nonnegative (default 0), and events are ordered newest first with UUID tie-breaking. Actor/target IDs may be null for events without that reference; credential secrets are never exposed in details.

Both authenticated roles can read users, groups, nodes, desired configuration and audit events without credential fields. Authenticated mutations require an administrator except logout and changing one's own password. Mutations require the canonical configured HTTPS Origin and the CSRF token returned by the same session's login; login itself requires Origin but no prior session or CSRF. Cookies are HttpOnly, Secure, SameSite=Lax, host-only, path `/`, and expire after 12 hours. Login hashing is serialized. Invalid credentials receive up to 60 ordinary authentication responses per minute before further invalid attempts are rate-limited; valid credentials remain admissible, avoiding a global unauthenticated lockout. Passwords use salted Argon2id, 32 MiB, two iterations, one lane; verification uses constant-time digest comparison. The service persists SHA-256 session/CSRF digests rather than bearer secrets. Successful login and authenticated index access perform bounded expired-session cleanup; rejected login does not reclaim sessions. UI responses set CSP `frame-ancestors 'none'` and `X-Frame-Options: DENY`. No request bodies or secrets are logged. A private 32-byte `secrets.key` is created on startup for later encrypted-secret storage; back it up securely, never commit it.

## Architecture and persistence

- `cmd/orbitbeacon-server`: command parsing, local bootstrap and HTTP lifecycle.
- `internal/config`: source precedence, validation and immutable installation policy conflict detection.
- `internal/server/storage`: local-file SQLite and versioned transactional migrations.
- `internal/server/auth`: user credentials, bounded login work and session lifecycle.
- `internal/server/management`: group management and paginated audit reads.
- `internal/server/nodes`: capacity reservation, asset operations, desired configuration and per-node batch results.
- `internal/server/http.go`: credential-free DTOs, authorization, Origin/CSRF gates, structured errors and precise static routing.
- `internal/server/webui`: embedded minimal browser UI.

SQLite uses WAL, foreign keys, 5000 ms busy timeout, FULL synchronous mode, disabled mmap and 8 MiB cache per connection. The pool is capped at three connections. `BEGIN IMMEDIATE` serializes transactional writers before capacity checks. State changes and actor-attributed audit inserts share the same transaction: an audit insertion failure rolls back the change, rather than leaving an unaudited success. Node batches retain this guarantee independently per node and return a failure for a node whose transaction fails. Node deletion revokes credentials, cancels active update state and removes the asset atomically; historical update result node IDs intentionally survive asset deletion. Group deletion atomically clears node membership. The later ingestion/history slice will introduce its bounded single-writer queues and read-only connection routing; those are not simulated here. Assets are never deleted automatically because they go offline.

`modernc.org/sqlite` is pinned to **v1.61.0**, not the plan's v1.61.1: v1.61.1 was unavailable when the dependency was resolved. The available v1.61.0 requires Go 1.26 and is compatible with the required Go 1.27.3 toolchain.

## Command boundaries

All commands parse `--help`, reject unknown flags/positionals and return nonzero on errors.

```text
orbitbeacon-server serve --config FILE [--data-dir DIR] [--listen-addr ADDR] [--public-url URL] [--max-nodes N]
orbitbeacon-server bootstrap --data-dir DIR [--username USER]
orbitbeacon-server backup --config FILE --output FILE
orbitbeacon-server restore --data-dir DIR --input FILE

orbitbeacon-agent version [--json]
orbitbeacon-agent run --config FILE --install-policy FILE
orbitbeacon-agent enroll --config FILE --install-policy FILE --token-file FILE
orbitbeacon-agent doctor --config FILE --install-policy FILE [--json]
orbitbeacon-agent update --config FILE --install-policy FILE --version VERSION
```

Backup/restore deliberately return unsupported errors without modifying files. **Never hot-copy the `.db` file** as a backup: the later backup slice must provide a consistent snapshot and package all required keys/configuration. Agent `version` works. Other agent commands parse and validate configuration/policy, then report unavailable functionality; `doctor` can report completed configuration validation but exits nonzero because runtime diagnostics are not implemented. No monitoring/enrollment/update success is claimed.

The agent installation policy fixes `server_url`, `ca_file`, `state_dir`, `runtime_dir`, `expected_uid`, `expected_gid`, `expected_account`, `install_mode`, `trust_key_file`. Every supplied configuration source is checked against policy independently, even if a higher-precedence value would hide a conflict. The CLI checks POSIX effective UID/GID, rejects UID/GID 0 and mismatches, and requires a regular policy file not writable by group/others. Full administrator-owned ancestry validation, Windows identity enforcement and installers belong to the platform slice; these unsupported execution paths never launch an agent. Runtime work is not implemented in this delivery.

## Development checks

```sh
GOTOOLCHAIN=go1.27.3 gofmt -w cmd internal
GOTOOLCHAIN=go1.27.3 go test ./...
GOTOOLCHAIN=go1.27.3 go test -race ./internal/server/...
GOTOOLCHAIN=go1.27.3 go vet ./...
```

## License

The existing [`LICENSE`](LICENSE) is preserved: **GNU General Public License, version 3 (GPL-3.0)**. The plan's Apache/empty-repository assumption does not apply to this repository.
