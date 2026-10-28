package storage

const notificationsSchema = `
CREATE TABLE notification_channels (
 id TEXT PRIMARY KEY, name TEXT NOT NULL CHECK(length(name) BETWEEN 1 AND 100),
 kind TEXT NOT NULL CHECK(kind IN ('webhook','telegram','email')),
 enabled INTEGER NOT NULL CHECK(enabled IN (0,1)), config_json TEXT NOT NULL,
 secret_ciphertext BLOB, url_ciphertext BLOB, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE TABLE notification_rate_limits (
 channel_id TEXT PRIMARY KEY REFERENCES notification_channels(id) ON DELETE CASCADE,
 window_start TEXT NOT NULL, count INTEGER NOT NULL CHECK(count BETWEEN 0 AND 20)
);
CREATE TABLE alert_rule_channels (
 rule_id TEXT NOT NULL REFERENCES alert_rules(id) ON DELETE CASCADE,
 channel_id TEXT NOT NULL REFERENCES notification_channels(id) ON DELETE CASCADE,
 PRIMARY KEY(rule_id,channel_id)
);
CREATE INDEX alert_rule_channels_channel ON alert_rule_channels(channel_id);
CREATE TABLE notification_deliveries (
 id TEXT PRIMARY KEY, channel_id TEXT NOT NULL REFERENCES notification_channels(id) ON DELETE CASCADE,
 event_id TEXT NOT NULL, state TEXT NOT NULL CHECK(state IN ('pending','sent','failed')),
 attempts INTEGER NOT NULL DEFAULT 0 CHECK(attempts BETWEEN 0 AND 5),
 next_attempt_at TEXT NOT NULL, last_error_code TEXT NOT NULL DEFAULT '' CHECK(length(last_error_code)<=40),
 lease_until TEXT NOT NULL DEFAULT '', payload_json TEXT NOT NULL, coalesced_count INTEGER NOT NULL DEFAULT 1,
 created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
 UNIQUE(channel_id,event_id,payload_json)
);
CREATE INDEX notification_deliveries_due ON notification_deliveries(next_attempt_at,id) WHERE state='pending';
CREATE INDEX notification_deliveries_channel ON notification_deliveries(channel_id,state,next_attempt_at);
CREATE INDEX notification_deliveries_retention ON notification_deliveries(updated_at,id) WHERE state IN ('sent','failed');
CREATE INDEX notification_deliveries_history ON notification_deliveries(created_at DESC,id DESC);
`
