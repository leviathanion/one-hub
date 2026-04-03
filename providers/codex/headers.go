package codex

import "strings"

type codexHeaderEntry struct {
	key   string
	value string
}

type codexHeaderBag struct {
	headers map[string]codexHeaderEntry
}

func newCodexHeaderBag() *codexHeaderBag {
	return &codexHeaderBag{
		headers: make(map[string]codexHeaderEntry),
	}
}

func newCodexHeaderBagFromMap(headers map[string]string) *codexHeaderBag {
	bag := newCodexHeaderBag()
	for key, value := range headers {
		bag.Set(key, value)
	}
	return bag
}

func normalizeCodexHeaderKey(key string) string {
	return strings.ToLower(strings.TrimSpace(key))
}

func (b *codexHeaderBag) Set(key, value string) {
	if b == nil {
		return
	}

	normalizedKey := normalizeCodexHeaderKey(key)
	if normalizedKey == "" {
		return
	}

	if strings.TrimSpace(value) == "" {
		delete(b.headers, normalizedKey)
		return
	}

	trimmedKey := strings.TrimSpace(key)
	if trimmedKey == "" {
		trimmedKey = key
	}

	b.headers[normalizedKey] = codexHeaderEntry{
		key:   trimmedKey,
		value: value,
	}
}

func (b *codexHeaderBag) SetIfAbsent(key, value string) {
	if b == nil || b.Has(key) {
		return
	}
	b.Set(key, value)
}

func (b *codexHeaderBag) Delete(key string) {
	if b == nil {
		return
	}
	delete(b.headers, normalizeCodexHeaderKey(key))
}

func (b *codexHeaderBag) Get(key string) string {
	if b == nil {
		return ""
	}
	entry, ok := b.headers[normalizeCodexHeaderKey(key)]
	if !ok {
		return ""
	}
	return strings.TrimSpace(entry.value)
}

func (b *codexHeaderBag) Has(key string) bool {
	return b.Get(key) != ""
}

func (b *codexHeaderBag) Map() map[string]string {
	if b == nil {
		return nil
	}

	headers := make(map[string]string, len(b.headers))
	for _, entry := range b.headers {
		headers[entry.key] = entry.value
	}
	return headers
}
