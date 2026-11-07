package storage

const availabilitySchema = `
CREATE TABLE node_availability_daily (
 node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
 day TEXT NOT NULL,
 online_seconds INTEGER NOT NULL CHECK(online_seconds BETWEEN 0 AND 86400),
 offline_seconds INTEGER NOT NULL CHECK(offline_seconds BETWEEN 0 AND 86400),
 CHECK(online_seconds + offline_seconds <= 86400),
 PRIMARY KEY(node_id, day)
) WITHOUT ROWID;
CREATE INDEX node_availability_daily_retention ON node_availability_daily(day);
`
