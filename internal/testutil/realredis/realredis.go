package realredis

import (
	"context"
	"errors"
	"os"
	"strings"

	"github.com/redis/go-redis/v9"
)

const URLVariable = "ONE_HUB_TEST_REDIS_URL"

// OpenFromEnvironment opens the external Redis-compatible server selected for
// Lua integration tests. The caller owns the returned client. An unset URL is
// reported separately so ordinary unit-test runs can skip the integration.
func OpenFromEnvironment(ctx context.Context) (*redis.Client, bool, error) {
	rawURL := strings.TrimSpace(os.Getenv(URLVariable))
	if rawURL == "" {
		return nil, false, nil
	}
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		return nil, true, errors.New("invalid Redis integration test URL")
	}
	options.Protocol = 2
	options.DisableIdentity = true
	options.MaxRetries = 0
	client := redis.NewClient(options)
	if ctx == nil {
		ctx = context.Background()
	}
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, true, err
	}
	return client, true, nil
}
