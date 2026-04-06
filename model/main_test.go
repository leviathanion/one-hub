package model

import (
	"strings"
	"testing"
	"time"

	"one-api/common"
)

func TestMysqlSessionTimezoneValueUsesCurrentOffset(t *testing.T) {
	location := time.FixedZone("custom", -(5*3600 + 30*60))
	got := mysqlSessionTimezoneValue(time.Unix(0, 0), location)
	if got != "'-05:30'" {
		t.Fatalf("expected mysql session timezone offset to be quoted, got %s", got)
	}
}

func TestDsnAddArgEscapesMysqlStringSystemVariables(t *testing.T) {
	originalUsingPostgreSQL := common.UsingPostgreSQL
	common.UsingPostgreSQL = false
	t.Cleanup(func() {
		common.UsingPostgreSQL = originalUsingPostgreSQL
	})

	got := dsnAddArg("user:password@tcp(localhost:3306)/dbname", "time_zone", "'+08:00'")
	if !strings.Contains(got, "time_zone=%27%2B08%3A00%27") {
		t.Fatalf("expected mysql dsn string system variables to be query-escaped, got %s", got)
	}
}
