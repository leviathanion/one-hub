package requester

// WSActivityConn captures the subset of *websocket.Conn used by
// InstallWSActivityHandlers.
type WSActivityConn interface {
	PingHandler() func(string) error
	SetPingHandler(func(string) error)
	PongHandler() func(string) error
	SetPongHandler(func(string) error)
}

// InstallWSActivityHandlers wraps the existing ping/pong handlers on conn so
// that onActivity runs before each handler. The original handler return value
// is preserved; if conn has no prior handler (handler is nil) the wrapper
// returns nil. Safe to invoke multiple times — each wrap chains the previous
// handler.
func InstallWSActivityHandlers(conn WSActivityConn, onActivity func()) {
	if conn == nil {
		return
	}
	previousPing := conn.PingHandler()
	conn.SetPingHandler(func(appData string) error {
		if onActivity != nil {
			onActivity()
		}
		if previousPing != nil {
			return previousPing(appData)
		}
		return nil
	})
	previousPong := conn.PongHandler()
	conn.SetPongHandler(func(appData string) error {
		if onActivity != nil {
			onActivity()
		}
		if previousPong != nil {
			return previousPong(appData)
		}
		return nil
	})
}
