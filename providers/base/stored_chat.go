package base

// StoredChatSupport 把原生 Chat 交付接到资源归属屏障。
// adapter 只提取真实 ID，relay 负责 SQL 授权及提交。
type StoredChatSupport interface {
	SupportsStoredChat() bool
	SetStoredChatOwnerCommit(func(string) error)
}
