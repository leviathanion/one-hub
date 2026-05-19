package requester

type wsReadLimitConn interface {
	SetReadLimit(int64)
}

// ApplyWSReadLimit applies a configured websocket read limit to conn and
// returns the value that was installed. gorilla/websocket does not report an
// error from SetReadLimit, so callers keep ownership of close/error policy.
func ApplyWSReadLimit(conn wsReadLimitConn, readLimit func() int64) int64 {
	if conn == nil {
		return 0
	}
	if readLimit == nil {
		readLimit = defaultWSReadLimit
	}
	limit := readLimit()
	if limit <= 0 {
		limit = defaultWSReadLimit()
	}
	conn.SetReadLimit(limit)
	return limit
}

func defaultWSReadLimit() int64 {
	return 16 << 20
}
