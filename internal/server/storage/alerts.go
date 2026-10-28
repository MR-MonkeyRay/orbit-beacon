package storage

// Generated foreign keys enforce polymorphic scope ownership. Event snapshots
// retain history after rule or inventory deletion so open episodes can resolve.
const alertsSchema = `
CREATE TABLE alert_rules (
 id TEXT PRIMARY KEY, name TEXT NOT NULL CHECK(length(name) BETWEEN 1 AND 100),
 enabled INTEGER NOT NULL CHECK(enabled IN (0,1)),
 scope_kind TEXT NOT NULL CHECK(scope_kind IN ('all','group','node')), scope_id TEXT,
 scope_group_id TEXT GENERATED ALWAYS AS (CASE WHEN scope_kind='group' THEN scope_id END) VIRTUAL REFERENCES groups(id) ON DELETE CASCADE,
 scope_node_id TEXT GENERATED ALWAYS AS (CASE WHEN scope_kind='node' THEN scope_id END) VIRTUAL REFERENCES nodes(id) ON DELETE CASCADE,
 metric TEXT NOT NULL CHECK(metric IN ('cpu_percent','memory_used_percent','swap_used_percent','disk_used_percent','load1','offline')),
 comparator TEXT, threshold REAL, for_seconds INTEGER NOT NULL CHECK(for_seconds BETWEEN 0 AND 86400),
 severity TEXT NOT NULL CHECK(severity IN ('info','warning','critical')),
 created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
 CHECK((scope_kind='all' AND scope_id IS NULL) OR (scope_kind IN ('group','node') AND scope_id IS NOT NULL)),
 CHECK((metric='offline' AND comparator IS NULL AND threshold IS NULL) OR
       (metric!='offline' AND comparator IS NOT NULL AND comparator IN ('>','<') AND threshold IS NOT NULL AND threshold>=0 AND
        ((metric='load1' AND threshold<=1000000) OR (metric!='load1' AND threshold<=100))))
);
CREATE INDEX alert_rules_group ON alert_rules(scope_group_id);
CREATE INDEX alert_rules_node ON alert_rules(scope_node_id);
CREATE TABLE alert_events (
 id TEXT PRIMARY KEY, rule_id TEXT NOT NULL, node_id TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('firing','resolved')), value REAL,
 started_at TEXT NOT NULL, resolved_at TEXT, last_notified_at TEXT,
 rule_name TEXT NOT NULL, node_name TEXT NOT NULL, rule_snapshot TEXT NOT NULL,
 CHECK((state='firing' AND resolved_at IS NULL) OR (state='resolved' AND resolved_at IS NOT NULL))
);
CREATE UNIQUE INDEX alert_events_active ON alert_events(rule_id,node_id) WHERE state='firing';
CREATE INDEX alert_events_history ON alert_events(started_at DESC,id DESC);
CREATE INDEX alert_events_state_history ON alert_events(state,started_at DESC,id DESC);
CREATE INDEX alert_events_retention ON alert_events(resolved_at,id) WHERE state='resolved';
`
