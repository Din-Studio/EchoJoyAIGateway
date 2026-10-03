package dbtx

import (
	"testing"

	"gpt-load/internal/testutil/pgtest"
)

func TestMain(m *testing.M) {
	pgtest.Run(m, nil)
}
