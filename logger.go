package wado

import (
	"io"
	"log/slog"
)

// DiscardLogger is the default no-op logger: every record is dropped.
//
// Design constraint: the library never falls back to slog.Default(); log
// handling must be injected explicitly via WithLogger or WithLogHandler so
// the library never writes into the application's default log output.
var DiscardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))
