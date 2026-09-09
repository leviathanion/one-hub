package sqlitetest

import (
	"fmt"
	"sync/atomic"
)

var memoryDatabaseSequence atomic.Uint64

// MemoryDSN returns an isolated shared-cache SQLite database for one test setup.
// A fresh name on every call keeps repeated test runs in the same process from
// observing a still-open connection pool left by an earlier run.
func MemoryDSN() string {
	return fmt.Sprintf("file:one_hub_test_%d?mode=memory&cache=shared", memoryDatabaseSequence.Add(1))
}
