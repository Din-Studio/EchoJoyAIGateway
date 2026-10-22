package storage

import migrationfiles "gpt-load/internal/storage/migrations"

const lyMigrationLedgerTable = "ly_schema_migrations"

// lyMigrationTrack holds the Lingying billing migrations, which exist only on
// main. It runs after the core track and never takes core numbers, so dev
// merges into main without renumbering.
var lyMigrationTrack = migrationTrack{
	Ledger: lyMigrationLedgerTable,
	Prefix: "ly",
	Entries: []migration{
		{ID: migrationfiles.IDLy0001, Up: migrationfiles.UpLy0001, Validate: migrationfiles.ValidateLy0001},
	},
}
