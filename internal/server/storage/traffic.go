package storage

const trafficSchema = `
CREATE TABLE traffic_hourly (
 node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
 epoch TEXT NOT NULL,
 period_start TEXT NOT NULL,
 rx_bytes INTEGER NOT NULL CHECK(rx_bytes>=0),
 tx_bytes INTEGER NOT NULL CHECK(tx_bytes>=0),
 updated_at TEXT NOT NULL,
 PRIMARY KEY(node_id, epoch, period_start)
) WITHOUT ROWID;
CREATE INDEX traffic_hourly_retention ON traffic_hourly(period_start);
CREATE TABLE traffic_daily (
 node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
 day TEXT NOT NULL,
 rx_bytes INTEGER NOT NULL,
 tx_bytes INTEGER NOT NULL,
 PRIMARY KEY(node_id, day)
) WITHOUT ROWID;
CREATE INDEX traffic_daily_retention ON traffic_daily(day);
`
