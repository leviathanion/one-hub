package model

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"testing"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	sqliteDriver "github.com/mattn/go-sqlite3"
)

func TestIsRetryableDatabaseError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "bad connection", err: fmt.Errorf("wrapped: %w", driver.ErrBadConn), want: true},
		{name: "mysql deadlock", err: &mysqlDriver.MySQLError{Number: 1213}, want: true},
		{name: "mysql constraint", err: &mysqlDriver.MySQLError{Number: 1062}, want: false},
		{name: "postgres serialization", err: &pgconn.PgError{Code: "40001"}, want: true},
		{name: "postgres constraint", err: &pgconn.PgError{Code: "23505"}, want: false},
		{name: "sqlite busy", err: sqliteDriver.Error{Code: sqliteDriver.ErrBusy}, want: true},
		{name: "sqlite constraint", err: sqliteDriver.Error{Code: sqliteDriver.ErrConstraint}, want: false},
		{name: "deadline", err: context.DeadlineExceeded, want: false},
		{name: "permanent", err: errors.New("schema mismatch"), want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsRetryableDatabaseError(test.err); got != test.want {
				t.Fatalf("IsRetryableDatabaseError(%v)=%v want %v", test.err, got, test.want)
			}
		})
	}
}
