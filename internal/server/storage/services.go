package storage

const servicesSchema = `
CREATE TABLE services (
 id TEXT PRIMARY KEY NOT NULL, name TEXT NOT NULL CHECK(length(name) BETWEEN 1 AND 100),
 type TEXT NOT NULL CHECK(type IN ('http','tcp','icmp')), target TEXT NOT NULL,
 interval_seconds INTEGER NOT NULL DEFAULT 60 CHECK(interval_seconds BETWEEN 30 AND 3600),
 timeout_seconds INTEGER NOT NULL DEFAULT 10 CHECK(timeout_seconds BETWEEN 1 AND 30 AND timeout_seconds<interval_seconds),
 enabled INTEGER NOT NULL CHECK(enabled IN (0,1)), public INTEGER NOT NULL CHECK(public IN (0,1)),
 display_order INTEGER NOT NULL DEFAULT 0, notify INTEGER NOT NULL CHECK(notify IN (0,1)),
 latency_threshold_ms REAL NOT NULL DEFAULT 0 CHECK(latency_threshold_ms>=0),
 created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE TABLE service_checks_latest (
 service_id TEXT PRIMARY KEY NOT NULL REFERENCES services(id) ON DELETE CASCADE,
 result_json TEXT NOT NULL
);`

const serviceHistorySchema = `
CREATE TABLE service_check_buckets (
 service_id TEXT NOT NULL, t INTEGER NOT NULL, checks INTEGER NOT NULL CHECK(checks>0),
 successes INTEGER NOT NULL CHECK(successes>=0 AND successes<=checks),
 latency_sum_ms REAL NOT NULL CHECK(latency_sum_ms>=0),
 latency_min_ms REAL NOT NULL CHECK(latency_min_ms>=0), latency_max_ms REAL NOT NULL CHECK(latency_max_ms>=latency_min_ms),
 PRIMARY KEY(service_id,t)
) WITHOUT ROWID;
CREATE INDEX service_check_buckets_retention ON service_check_buckets(t,service_id);`
