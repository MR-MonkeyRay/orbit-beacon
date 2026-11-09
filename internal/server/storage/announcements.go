package storage

const announcementsSchema = `
CREATE TABLE announcements (
 id TEXT PRIMARY KEY NOT NULL,
 level TEXT NOT NULL CHECK(level IN ('info','warning','critical')),
 title TEXT NOT NULL,
 body TEXT NOT NULL,
 starts_at TEXT NOT NULL,
 ends_at TEXT,
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL,
 CHECK(ends_at IS NULL OR ends_at > starts_at)
);
CREATE INDEX announcements_window ON announcements(starts_at, ends_at);
`
