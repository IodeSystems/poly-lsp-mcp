package model

// Record is the persisted shape shared across the Go backend, the TypeScript
// client (client.ts), and the service config (config.yaml). Its LegacyID field
// is serialized as the JSON key `legacy_id`.
type Record struct {
	LegacyID string `json:"legacy_id"`
	Name     string `json:"name"`
}
