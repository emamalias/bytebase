package mssql

import (
	"context"
	"io"
)

// Dump dumps the database.
func (*Driver) Dump(_ context.Context, _ io.Writer, _ bool) (string, error) {
	// TODO(d): implement it.
	return "", nil
}
