# Record schema

The `Record` type exists in three synchronized representations. They MUST stay
in sync — a change to one is a change to all three.

| Representation | Location    | Field name  |
|----------------|-------------|-------------|
| Go struct      | `model.go`  | `LegacyID` (json tag `legacy_id`) |
| TypeScript     | `client.ts` | `legacyId`  |
| Config YAML    | `config.yaml` | `legacy_id` |

All three name the same logical field.
