# Changelog

## Unreleased

- Add the Go 1.27.3 foundation control plane: validated server configuration, immutable agent install-policy conflict checks, durable transactional SQLite migrations, local-only administrator bootstrap, Argon2id credentials and 12-hour secure sessions.
- Add administrator/viewer authorization, exact configured-Origin and session-bound CSRF enforcement, structured API errors, capacity-safe pending nodes, transactional delete/revoke/cancel, bounded per-node batches and versioned desired configuration.
- Add real server command parsing and serving, explicit unavailable backup/restore and agent runtime operations, an embedded minimal live browser UI and OpenAPI wire contract.
- Complete foundation user/group management, own-password changes with old-password proof and full session revocation, optional node metadata patches with nullable group membership and sorted array tags, and paginated audit reads for both authenticated roles. Protect the last administrator and revoke affected sessions on role changes.
- Require transactional actor-attributed audit insertion for state changes, rolling back on audit failure and preserving per-node batch failures. Detach nodes atomically when deleting a group.
- Canonicalize the configured HTTPS public origin, check unsupported methods before authentication, deny UI framing, avoid global login lockout while bounding invalid credential responses, perform bounded expired-session cleanup after successful login/index access, and validate private effective-user-owned data/key/SQLite files and writable local Linux storage.
- Expand the existing OpenAPI and README contracts for foundation authorization, request/response schemas, pagination, security and persistence without claiming later monitoring or agent features.
- Pin modernc.org/sqlite v1.61.0 because the planned v1.61.1 was unavailable; retain the repository's existing GPL-3.0 license instead of the plan's Apache assumption.
