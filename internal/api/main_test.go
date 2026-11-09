package api

import (
	"context"
	"os"
	"testing"

	"github.com/xrdavies/mobd/internal/desktop"
)

func TestMain(m *testing.M) {
	// On a Mac /health must not read the real home folders or send to the router from tests.
	currentGrants = func(context.Context) []desktop.Grant { return nil }
	os.Exit(m.Run())
}
