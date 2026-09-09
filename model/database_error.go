package model

import (
	"context"
	"database/sql/driver"
	"errors"
	"net"
	"strings"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	sqliteDriver "github.com/mattn/go-sqlite3"
)

// IsRetryableDatabaseError recognizes transient failures for short, bounded,
// idempotent database operations. It must not be used to replay writes whose
// identity or commit semantics are ambiguous.
func IsRetryableDatabaseError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, driver.ErrBadConn) {
		return true
	}

	var mysqlErr *mysqlDriver.MySQLError
	if errors.As(err, &mysqlErr) {
		switch mysqlErr.Number {
		case 1040, 1203, 1205, 1213, 2006, 2013:
			return true
		}
		return false
	}

	var postgresErr *pgconn.PgError
	if errors.As(err, &postgresErr) {
		code := postgresErr.Code
		return strings.HasPrefix(code, "08") || code == "40001" || code == "40P01" || code == "55P03" || strings.HasPrefix(code, "57P0")
	}

	var sqliteErr sqliteDriver.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.Code == sqliteDriver.ErrBusy || sqliteErr.Code == sqliteDriver.ErrLocked
	}

	var networkErr net.Error
	return errors.As(err, &networkErr) && (networkErr.Timeout() || networkErr.Temporary())
}

func IsUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}
	var mysqlErr *mysqlDriver.MySQLError
	if errors.As(err, &mysqlErr) {
		return mysqlErr.Number == 1062
	}
	var postgresErr *pgconn.PgError
	if errors.As(err, &postgresErr) {
		return postgresErr.Code == "23505"
	}
	var sqliteErr sqliteDriver.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.Code == sqliteDriver.ErrConstraint
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unique constraint") || strings.Contains(message, "duplicate key")
}
