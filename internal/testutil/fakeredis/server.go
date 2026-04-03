package fakeredis

import (
	"bufio"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/redis/go-redis/v9"
)

type Server struct {
	listener    net.Listener
	mu          sync.Mutex
	values      map[string]string
	zsets       map[string]map[string]float64
	scriptBySHA map[string]func(keys, args []string) int64
	failNext    map[string][]string
	closed      bool
}

type Binding struct {
	Key               string `json:"Key"`
	SessionKey        string `json:"SessionKey"`
	Scope             string `json:"Scope"`
	SessionID         string `json:"SessionID"`
	CallerNS          string `json:"CallerNS"`
	ChannelID         int    `json:"ChannelID"`
	CompatibilityHash string `json:"CompatibilityHash"`
}

type SessionScriptHashes struct {
	CreateBindingIfAbsent                string
	ReplaceBindingIfSessionMatches       string
	DeleteBindingIfSessionMatches        string
	TouchBindingIfSessionMatches         string
	DeleteBindingAndRevokeIfSessionMatch string
}

func Start() (*Server, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	server := &Server{
		listener:    listener,
		values:      make(map[string]string),
		zsets:       make(map[string]map[string]float64),
		scriptBySHA: make(map[string]func(keys, args []string) int64),
		failNext:    make(map[string][]string),
	}
	go server.serve()
	return server, nil
}

func (s *Server) Addr() string {
	if s == nil || s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

func (s *Server) Close() error {
	if s == nil || s.listener == nil {
		return nil
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return s.listener.Close()
}

func (s *Server) Client() *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:            s.Addr(),
		Protocol:        2,
		DisableIdentity: true,
		MaxRetries:      0,
	})
}

func (s *Server) SetRaw(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[key] = value
}

func (s *Server) GetRaw(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[key]
	return value, ok
}

func (s *Server) RegisterLuaScript(script string, handler func(keys, args []string) int64) string {
	hash := sha1Hex(script)
	s.RegisterScriptHash(hash, handler)
	return hash
}

func (s *Server) RegisterScriptHash(hash string, handler func(keys, args []string) int64) string {
	s.mu.Lock()
	s.scriptBySHA[hash] = handler
	s.mu.Unlock()
	return hash
}

func (s *Server) FailNext(command, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	command = strings.ToUpper(strings.TrimSpace(command))
	if command == "" {
		return
	}
	s.failNext[command] = append(s.failNext[command], message)
}

func (s *Server) RegisterSessionBindingScripts(hashes SessionScriptHashes) {
	if strings.TrimSpace(hashes.CreateBindingIfAbsent) != "" {
		s.RegisterScriptHash(hashes.CreateBindingIfAbsent, func(keys, args []string) int64 {
			currentRaw, ok := s.GetRaw(keys[0])
			if !ok {
				s.SetRaw(keys[0], args[0])
				return 1
			}
			if bindingEquivalent(s.decodeBinding(currentRaw), s.decodeBinding(args[0])) {
				return 1
			}
			return 2
		})
	}
	if strings.TrimSpace(hashes.ReplaceBindingIfSessionMatches) != "" {
		s.RegisterScriptHash(hashes.ReplaceBindingIfSessionMatches, func(keys, args []string) int64 {
			currentRaw, ok := s.GetRaw(keys[0])
			if !ok {
				return 2
			}
			current := s.decodeBinding(currentRaw)
			replacement := s.decodeBinding(args[0])
			if bindingEquivalent(current, replacement) {
				return 1
			}
			if current != nil && current.SessionKey == args[1] {
				s.SetRaw(keys[0], args[0])
				return 1
			}
			return 2
		})
	}
	if strings.TrimSpace(hashes.DeleteBindingIfSessionMatches) != "" {
		s.RegisterScriptHash(hashes.DeleteBindingIfSessionMatches, func(keys, args []string) int64 {
			currentRaw, ok := s.GetRaw(keys[0])
			if !ok {
				return 1
			}
			current := s.decodeBinding(currentRaw)
			if current != nil && current.SessionKey == args[0] {
				s.mu.Lock()
				delete(s.values, keys[0])
				s.mu.Unlock()
				return 1
			}
			return 2
		})
	}
	if strings.TrimSpace(hashes.TouchBindingIfSessionMatches) != "" {
		s.RegisterScriptHash(hashes.TouchBindingIfSessionMatches, func(keys, args []string) int64 {
			currentRaw, ok := s.GetRaw(keys[0])
			if !ok {
				return 2
			}
			current := s.decodeBinding(currentRaw)
			if current != nil && current.SessionKey == args[0] {
				return 1
			}
			return 2
		})
	}
	if strings.TrimSpace(hashes.DeleteBindingAndRevokeIfSessionMatch) != "" {
		s.RegisterScriptHash(hashes.DeleteBindingAndRevokeIfSessionMatch, func(keys, args []string) int64 {
			currentRaw, ok := s.GetRaw(keys[0])
			if !ok {
				return 2
			}
			current := s.decodeBinding(currentRaw)
			if current != nil && current.SessionKey == args[0] {
				s.mu.Lock()
				delete(s.values, keys[0])
				s.values[keys[1]] = "1"
				s.mu.Unlock()
				return 1
			}
			return 2
		})
	}
}

func (s *Server) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return
			}
			continue
		}
		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)

	for {
		command, err := readCommand(reader)
		if err != nil {
			if err == io.EOF {
				return
			}
			writeError(writer, "ERR invalid command")
			_ = writer.Flush()
			return
		}
		if len(command) == 0 {
			writeError(writer, "ERR empty command")
			_ = writer.Flush()
			continue
		}

		if err := s.execute(strings.ToUpper(command[0]), command[1:], writer); err != nil {
			writeError(writer, err.Error())
		}
		if err := writer.Flush(); err != nil {
			return
		}
	}
}

func (s *Server) execute(command string, args []string, writer *bufio.Writer) error {
	if err, failed := s.consumeFailure(command); failed {
		return fmt.Errorf("%s", err)
	}

	switch command {
	case "HELLO":
		return fmt.Errorf("ERR unknown command `HELLO`")
	case "PING":
		writeSimpleString(writer, "PONG")
		return nil
	case "AUTH", "SELECT", "CLIENT":
		writeSimpleString(writer, "OK")
		return nil
	case "GET":
		if len(args) < 1 {
			return fmt.Errorf("ERR wrong number of arguments for 'get'")
		}
		value, ok := s.GetRaw(args[0])
		if !ok {
			writeNilBulkString(writer)
			return nil
		}
		writeBulkString(writer, value)
		return nil
	case "SET":
		if len(args) < 2 {
			return fmt.Errorf("ERR wrong number of arguments for 'set'")
		}
		s.SetRaw(args[0], args[1])
		writeSimpleString(writer, "OK")
		return nil
	case "DEL":
		deleted := int64(0)
		s.mu.Lock()
		for _, key := range args {
			if _, ok := s.values[key]; ok {
				delete(s.values, key)
				deleted++
			} else if _, ok := s.zsets[key]; ok {
				delete(s.zsets, key)
				deleted++
			}
		}
		s.mu.Unlock()
		writeInteger(writer, deleted)
		return nil
	case "EXISTS":
		count := int64(0)
		s.mu.Lock()
		for _, key := range args {
			if _, ok := s.values[key]; ok {
				count++
				continue
			}
			if _, ok := s.zsets[key]; ok {
				count++
			}
		}
		s.mu.Unlock()
		writeInteger(writer, count)
		return nil
	case "PEXPIRE":
		if len(args) < 2 {
			return fmt.Errorf("ERR wrong number of arguments for 'pexpire'")
		}
		if s.keyExists(args[0]) {
			writeInteger(writer, 1)
		} else {
			writeInteger(writer, 0)
		}
		return nil
	case "SCAN":
		if len(args) < 1 {
			return fmt.Errorf("ERR wrong number of arguments for 'scan'")
		}
		cursor := args[0]
		pattern := "*"
		for i := 1; i+1 < len(args); i += 2 {
			if strings.EqualFold(args[i], "MATCH") {
				pattern = args[i+1]
			}
		}
		keys := s.scan(pattern)
		if cursor != "0" {
			keys = nil
		}
		writeScanReply(writer, "0", keys)
		return nil
	case "ZADD":
		if len(args) < 3 || len(args)%2 == 0 {
			return fmt.Errorf("ERR wrong number of arguments for 'zadd'")
		}
		key := args[0]
		added := int64(0)
		s.mu.Lock()
		members := s.zsets[key]
		if members == nil {
			members = make(map[string]float64)
			s.zsets[key] = members
		}
		delete(s.values, key)
		for i := 1; i+1 < len(args); i += 2 {
			score, err := strconv.ParseFloat(args[i], 64)
			if err != nil {
				s.mu.Unlock()
				return fmt.Errorf("ERR value is not a valid float")
			}
			member := args[i+1]
			if _, ok := members[member]; !ok {
				added++
			}
			members[member] = score
		}
		s.mu.Unlock()
		writeInteger(writer, added)
		return nil
	case "ZREM":
		if len(args) < 2 {
			return fmt.Errorf("ERR wrong number of arguments for 'zrem'")
		}
		removed := int64(0)
		s.mu.Lock()
		members := s.zsets[args[0]]
		for _, member := range args[1:] {
			if members == nil {
				break
			}
			if _, ok := members[member]; ok {
				delete(members, member)
				removed++
			}
		}
		if len(members) == 0 {
			delete(s.zsets, args[0])
		}
		s.mu.Unlock()
		writeInteger(writer, removed)
		return nil
	case "ZCARD":
		if len(args) < 1 {
			return fmt.Errorf("ERR wrong number of arguments for 'zcard'")
		}
		s.mu.Lock()
		count := int64(len(s.zsets[args[0]]))
		s.mu.Unlock()
		writeInteger(writer, count)
		return nil
	case "ZRANGE":
		if len(args) < 3 {
			return fmt.Errorf("ERR wrong number of arguments for 'zrange'")
		}
		start, err := strconv.Atoi(args[1])
		if err != nil {
			return fmt.Errorf("ERR value is not an integer or out of range")
		}
		stop, err := strconv.Atoi(args[2])
		if err != nil {
			return fmt.Errorf("ERR value is not an integer or out of range")
		}
		members := s.zrange(args[0], start, stop)
		writeArrayHeader(writer, len(members))
		for _, member := range members {
			writeBulkString(writer, member)
		}
		return nil
	case "ZRANGEBYSCORE":
		if len(args) < 3 {
			return fmt.Errorf("ERR wrong number of arguments for 'zrangebyscore'")
		}
		minScore, err := parseZScoreBound(args[1])
		if err != nil {
			return err
		}
		maxScore, err := parseZScoreBound(args[2])
		if err != nil {
			return err
		}
		offset := 0
		count := -1
		for i := 3; i < len(args); i++ {
			if !strings.EqualFold(args[i], "LIMIT") || i+2 >= len(args) {
				continue
			}
			offset, err = strconv.Atoi(args[i+1])
			if err != nil {
				return fmt.Errorf("ERR value is not an integer or out of range")
			}
			count, err = strconv.Atoi(args[i+2])
			if err != nil {
				return fmt.Errorf("ERR value is not an integer or out of range")
			}
			break
		}
		members := s.zrangeByScore(args[0], minScore, maxScore, offset, count)
		writeArrayHeader(writer, len(members))
		for _, member := range members {
			writeBulkString(writer, member)
		}
		return nil
	case "EVALSHA":
		if len(args) < 2 {
			return fmt.Errorf("ERR wrong number of arguments for 'evalsha'")
		}
		sha := args[0]
		numKeys, err := strconv.Atoi(args[1])
		if err != nil {
			return fmt.Errorf("ERR invalid numkeys")
		}
		if len(args) < 2+numKeys {
			return fmt.Errorf("ERR wrong number of arguments for 'evalsha'")
		}
		keys := append([]string(nil), args[2:2+numKeys]...)
		scriptArgs := append([]string(nil), args[2+numKeys:]...)
		s.mu.Lock()
		handler := s.scriptBySHA[sha]
		s.mu.Unlock()
		if handler == nil {
			return fmt.Errorf("NOSCRIPT No matching script. Please use EVAL.")
		}
		writeInteger(writer, handler(keys, scriptArgs))
		return nil
	case "EVAL":
		if len(args) < 2 {
			return fmt.Errorf("ERR wrong number of arguments for 'eval'")
		}
		script := args[0]
		numKeys, err := strconv.Atoi(args[1])
		if err != nil {
			return fmt.Errorf("ERR invalid numkeys")
		}
		if len(args) < 2+numKeys {
			return fmt.Errorf("ERR wrong number of arguments for 'eval'")
		}
		keys := append([]string(nil), args[2:2+numKeys]...)
		scriptArgs := append([]string(nil), args[2+numKeys:]...)
		if handler := s.handlerForScript(script); handler != nil {
			writeInteger(writer, handler(keys, scriptArgs))
			return nil
		}
		return fmt.Errorf("ERR unsupported script")
	default:
		return fmt.Errorf("ERR unknown command `%s`", command)
	}
}

func (s *Server) handlerForScript(script string) func(keys, args []string) int64 {
	hash := sha1Hex(script)
	s.mu.Lock()
	handler := s.scriptBySHA[hash]
	s.mu.Unlock()
	if handler != nil {
		return handler
	}

	script = strings.ReplaceAll(script, "\n", " ")
	script = strings.Join(strings.Fields(script), " ")
	switch {
	case strings.Contains(script, "redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])") && !strings.Contains(script, "KEYS[2]"):
		return func(keys, args []string) int64 {
			currentRaw, ok := s.GetRaw(keys[0])
			if !ok {
				s.SetRaw(keys[0], args[0])
				return 1
			}
			if bindingEquivalent(s.decodeBinding(currentRaw), s.decodeBinding(args[0])) {
				return 1
			}
			return 2
		}
	case strings.Contains(script, "currentBinding and currentBinding.SessionKey == ARGV[2]"):
		return func(keys, args []string) int64 {
			currentRaw, ok := s.GetRaw(keys[0])
			if !ok {
				return 2
			}
			current := s.decodeBinding(currentRaw)
			replacement := s.decodeBinding(args[0])
			if bindingEquivalent(current, replacement) {
				return 1
			}
			if current != nil && current.SessionKey == args[1] {
				s.SetRaw(keys[0], args[0])
				return 1
			}
			return 2
		}
	case strings.Contains(script, "redis.call('SET', KEYS[2], '1', 'PX', ARGV[2])"):
		return func(keys, args []string) int64 {
			currentRaw, ok := s.GetRaw(keys[0])
			if !ok {
				return 2
			}
			current := s.decodeBinding(currentRaw)
			if current != nil && current.SessionKey == args[0] {
				s.mu.Lock()
				delete(s.values, keys[0])
				s.values[keys[1]] = "1"
				s.mu.Unlock()
				return 1
			}
			return 2
		}
	case strings.Contains(script, "redis.call('PEXPIRE', KEYS[1], ARGV[2])"):
		return func(keys, args []string) int64 {
			currentRaw, ok := s.GetRaw(keys[0])
			if !ok {
				return 2
			}
			current := s.decodeBinding(currentRaw)
			if current != nil && current.SessionKey == args[0] {
				return 1
			}
			return 2
		}
	case strings.Contains(script, "redis.call('DEL', KEYS[1])"):
		return func(keys, args []string) int64 {
			currentRaw, ok := s.GetRaw(keys[0])
			if !ok {
				return 1
			}
			current := s.decodeBinding(currentRaw)
			if current != nil && current.SessionKey == args[0] {
				s.mu.Lock()
				delete(s.values, keys[0])
				s.mu.Unlock()
				return 1
			}
			return 2
		}
	default:
		return nil
	}
}

func (s *Server) consumeFailure(command string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	queue := s.failNext[command]
	if len(queue) == 0 {
		return "", false
	}
	err := queue[0]
	if len(queue) == 1 {
		delete(s.failNext, command)
	} else {
		s.failNext[command] = queue[1:]
	}
	if strings.TrimSpace(err) == "" {
		err = "ERR forced failure"
	}
	return err, true
}

func (s *Server) scan(pattern string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.values)+len(s.zsets))
	if strings.HasSuffix(pattern, "*") {
		prefix := strings.TrimSuffix(pattern, "*")
		for key := range s.values {
			if strings.HasPrefix(key, prefix) {
				keys = append(keys, key)
			}
		}
		for key := range s.zsets {
			if strings.HasPrefix(key, prefix) {
				keys = append(keys, key)
			}
		}
	} else {
		for key := range s.values {
			if key == pattern {
				keys = append(keys, key)
			}
		}
		for key := range s.zsets {
			if key == pattern {
				keys = append(keys, key)
			}
		}
	}
	sort.Strings(keys)
	return keys
}

func (s *Server) keyExists(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.values[key]
	if ok {
		return true
	}
	_, ok = s.zsets[key]
	return ok
}

type zsetMember struct {
	member string
	score  float64
}

func (s *Server) orderedZSetMembers(key string) []zsetMember {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw := s.zsets[key]
	if len(raw) == 0 {
		return nil
	}
	members := make([]zsetMember, 0, len(raw))
	for member, score := range raw {
		members = append(members, zsetMember{member: member, score: score})
	}
	sort.Slice(members, func(i, j int) bool {
		if members[i].score == members[j].score {
			return members[i].member < members[j].member
		}
		return members[i].score < members[j].score
	})
	return members
}

func (s *Server) zrange(key string, start, stop int) []string {
	members := s.orderedZSetMembers(key)
	if len(members) == 0 {
		return nil
	}
	if start < 0 {
		start = len(members) + start
	}
	if stop < 0 {
		stop = len(members) + stop
	}
	if start < 0 {
		start = 0
	}
	if stop >= len(members) {
		stop = len(members) - 1
	}
	if start > stop || start >= len(members) {
		return nil
	}
	result := make([]string, 0, stop-start+1)
	for _, member := range members[start : stop+1] {
		result = append(result, member.member)
	}
	return result
}

func (s *Server) zrangeByScore(key string, minScore, maxScore float64, offset, count int) []string {
	members := s.orderedZSetMembers(key)
	if len(members) == 0 {
		return nil
	}
	if offset < 0 {
		offset = 0
	}
	result := make([]string, 0, len(members))
	for _, member := range members {
		if member.score < minScore || member.score > maxScore {
			continue
		}
		result = append(result, member.member)
	}
	if offset >= len(result) {
		return nil
	}
	result = result[offset:]
	if count >= 0 && count < len(result) {
		result = result[:count]
	}
	return result
}

func parseZScoreBound(raw string) (float64, error) {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case "-inf":
		return math.Inf(-1), nil
	case "+inf", "inf":
		return math.Inf(1), nil
	default:
		score, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return 0, fmt.Errorf("ERR min or max is not a float")
		}
		return score, nil
	}
}

func (s *Server) ResolveBindingPayload(key string) *Binding {
	raw, ok := s.GetRaw(key)
	if !ok {
		return nil
	}
	var payload struct {
		Binding Binding `json:"binding"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return nil
	}
	if strings.TrimSpace(payload.Binding.Key) == "" || strings.TrimSpace(payload.Binding.SessionKey) == "" {
		return nil
	}
	return &payload.Binding
}

func (s *Server) EqualBindingPayload(raw string, binding *Binding) bool {
	if binding == nil {
		return false
	}
	decoded := s.decodeBinding(raw)
	return bindingEquivalent(decoded, binding)
}

func (s *Server) decodeBinding(raw string) *Binding {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var payload struct {
		Binding Binding `json:"binding"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return nil
	}
	if strings.TrimSpace(payload.Binding.Key) == "" || strings.TrimSpace(payload.Binding.SessionKey) == "" {
		return nil
	}
	return &payload.Binding
}

func bindingEquivalent(current, replacement *Binding) bool {
	if current == nil || replacement == nil {
		return false
	}
	return current.Key == replacement.Key &&
		current.SessionKey == replacement.SessionKey &&
		current.Scope == replacement.Scope &&
		current.SessionID == replacement.SessionID &&
		current.CallerNS == replacement.CallerNS &&
		current.ChannelID == replacement.ChannelID &&
		current.CompatibilityHash == replacement.CompatibilityHash
}

func sha1Hex(payload string) string {
	sum := sha1.Sum([]byte(payload))
	return hex.EncodeToString(sum[:])
}

func readCommand(reader *bufio.Reader) ([]string, error) {
	prefix, err := reader.ReadByte()
	if err != nil {
		return nil, err
	}
	if prefix != '*' {
		return nil, fmt.Errorf("unexpected prefix %q", prefix)
	}
	countLine, err := readLine(reader)
	if err != nil {
		return nil, err
	}
	count, err := strconv.Atoi(countLine)
	if err != nil {
		return nil, err
	}
	command := make([]string, 0, count)
	for i := 0; i < count; i++ {
		bulkPrefix, err := reader.ReadByte()
		if err != nil {
			return nil, err
		}
		if bulkPrefix != '$' {
			return nil, fmt.Errorf("unexpected bulk prefix %q", bulkPrefix)
		}
		lengthLine, err := readLine(reader)
		if err != nil {
			return nil, err
		}
		length, err := strconv.Atoi(lengthLine)
		if err != nil {
			return nil, err
		}
		buffer := make([]byte, length+2)
		if _, err := io.ReadFull(reader, buffer); err != nil {
			return nil, err
		}
		command = append(command, string(buffer[:length]))
	}
	return command, nil
}

func readLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), nil
}

func writeSimpleString(writer *bufio.Writer, value string) {
	_, _ = writer.WriteString("+" + value + "\r\n")
}

func writeError(writer *bufio.Writer, value string) {
	_, _ = writer.WriteString("-" + value + "\r\n")
}

func writeInteger(writer *bufio.Writer, value int64) {
	_, _ = writer.WriteString(":" + strconv.FormatInt(value, 10) + "\r\n")
}

func writeBulkString(writer *bufio.Writer, value string) {
	_, _ = writer.WriteString("$" + strconv.Itoa(len(value)) + "\r\n" + value + "\r\n")
}

func writeNilBulkString(writer *bufio.Writer) {
	_, _ = writer.WriteString("$-1\r\n")
}

func writeArrayHeader(writer *bufio.Writer, size int) {
	_, _ = writer.WriteString("*" + strconv.Itoa(size) + "\r\n")
}

func writeScanReply(writer *bufio.Writer, cursor string, keys []string) {
	writeArrayHeader(writer, 2)
	writeBulkString(writer, cursor)
	writeArrayHeader(writer, len(keys))
	for _, key := range keys {
		writeBulkString(writer, key)
	}
}
