package app

import "time"

// Record is a stored domain entity.
type Record struct {
	ID        string
	CreatedAt time.Time
}

// Session tracks an authenticated login.
type Session struct {
	Token     string
	CreatedAt time.Time
}

// AuditEntry is an immutable audit-log line.
type AuditEntry struct {
	Actor     string
	Action    string
	CreatedAt time.Time
}
