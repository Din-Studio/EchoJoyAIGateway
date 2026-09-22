package requestlog

import (
	"math"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// incrementAssignments builds ON CONFLICT DO UPDATE assignments that add each
// non-negative amount to the existing row inside the database, so concurrent
// writers on a shared MySQL or PostgreSQL never lose each other's increments.
// The expression references the target table's current column on every
// supported dialect. Overflow writes the -1 sentinel, which the column's
// >= 0 CHECK constraint rejects and thereby rolls back the whole statement.
func incrementAssignments(amounts map[string]int64) map[string]any {
	updates := make(map[string]any, len(amounts))
	for name, amount := range amounts {
		column := clause.Column{Name: name, Table: clause.CurrentTable}
		updates[name] = gorm.Expr(
			"CASE WHEN ? > ? THEN -1 ELSE ? + ? END",
			column, math.MaxInt64-amount, column, amount,
		)
	}
	return updates
}
