package identity

import (
	"os"
	"testing"

	"gpt-load/internal/platform/i18n"
)

func TestMain(m *testing.M) {
	if err := i18n.Init(); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}
