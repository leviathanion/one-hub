package codex

import (
	commonredis "one-api/common/redis"
	runtimesession "one-api/runtime/session"
)

func GetExecutionSessionStats() runtimesession.Stats {
	codexExecutionSessions.ConfigureRedis(commonredis.GetRedisClient(), "one-hub:execution-session")
	return codexExecutionSessions.Stats()
}
