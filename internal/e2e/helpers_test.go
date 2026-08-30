//go:build e2e

package e2e_test

import (
	"io"
	"log/slog"
	"testing"

	"github.com/shuki/cprest/internal/destination"
	"github.com/shuki/cprest/internal/repobuild"
)

// testLogger keeps component logs out of passing test output while leaving
// them available with -v.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	if testing.Verbose() {
		return slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}

func buildDestination(opened repobuild.Opened) (destination.Destination, error) {
	return destination.Build(opened.Spec)
}
