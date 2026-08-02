// Record mirrors the Go `model.Record` struct. The `legacyId` field is the
// camelCase form of the `legacy_id` JSON key produced by the backend.
export interface Record {
  legacyId: string;
  name: string;
}
