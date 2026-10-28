package storage

const probesSchema = `
CREATE TABLE probe_latest (
 node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
 target TEXT NOT NULL,
 ok INTEGER NOT NULL CHECK(ok IN (0,1)),
 latency_ms REAL CHECK(latency_ms IS NULL OR latency_ms>=0),
 error TEXT NOT NULL CHECK(error IN ('','timeout','refused','dns','tls','http_status','unreachable','unsupported','error')),
 status_code INTEGER CHECK(status_code IS NULL OR status_code BETWEEN 100 AND 599),
 checked_at TEXT NOT NULL,
 PRIMARY KEY(node_id,target)
) WITHOUT ROWID;
CREATE TABLE probe_hourly (
 node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
 target TEXT NOT NULL,
 hour TEXT NOT NULL,
 checks INTEGER NOT NULL CHECK(checks>=0),
 failures INTEGER NOT NULL CHECK(failures>=0 AND failures<=checks),
 latency_sum_ms REAL NOT NULL CHECK(latency_sum_ms>=0),
 PRIMARY KEY(node_id,target,hour)
) WITHOUT ROWID;
CREATE INDEX probe_hourly_retention ON probe_hourly(hour);
`
