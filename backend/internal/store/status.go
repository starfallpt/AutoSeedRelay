package store

import "fmt"

// seeds.status and relay_records.status have no CHECK constraint in the schema
// (see migrations/00001_init.sql), and SQLite cannot ALTER an existing column
// to add one. The state machine (BIZ-SPEC §5) is therefore enforced here, in
// the application layer, at every Repo entry point that writes one of those two
// columns.
//
// NOTE: sources.status / targets.status already carry a DB CHECK
// (active/paused) and relay_records.role a CHECK (publisher/seeder), so they
// are intentionally not duplicated here.

// seedStatuses is the allowed set for seeds.status.
var seedStatuses = map[string]struct{}{
	"discovered":  {},
	"downloading": {},
	"downloaded":  {},
	"processing":  {},
	"seeding":     {},
	"retry":       {},
	"failed":      {},
	"retired":     {},
	"skipped":     {},
}

// recordStatuses is the allowed set for relay_records.status.
var recordStatuses = map[string]struct{}{
	"pending":          {},
	"uploading":        {},
	"published":        {},
	"cross_seeding":    {},
	"seeding":          {},
	"failed":           {},
	"retired":          {},
	"skipped_existing": {},
}

// validateSeedStatus returns an explicit error for any status outside the
// seeds.status whitelist.
func validateSeedStatus(status string) error {
	if _, ok := seedStatuses[status]; ok {
		return nil
	}
	return fmt.Errorf("store: invalid seed status %q", status)
}

// validateRecordStatus returns an explicit error for any status outside the
// relay_records.status whitelist.
func validateRecordStatus(status string) error {
	if _, ok := recordStatuses[status]; ok {
		return nil
	}
	return fmt.Errorf("store: invalid relay record status %q", status)
}
