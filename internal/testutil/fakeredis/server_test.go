package fakeredis

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func startTestServer(t *testing.T) *Server {
	t.Helper()

	server, err := Start()
	if err != nil {
		t.Fatalf("failed to start fake redis server: %v", err)
	}
	t.Cleanup(func() {
		_ = server.Close()
	})
	return server
}

func bindingJSON(t *testing.T, binding Binding) string {
	t.Helper()

	payload := struct {
		Binding Binding `json:"binding"`
	}{Binding: binding}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("failed to marshal binding payload: %v", err)
	}
	return string(raw)
}

func readRawReplyLine(t *testing.T, conn net.Conn) string {
	t.Helper()

	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("failed to set read deadline: %v", err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("failed to read server reply: %v", err)
	}
	return line
}

func TestServerClientCommandsAndSortedSets(t *testing.T) {
	server := startTestServer(t)
	if server.Addr() == "" {
		t.Fatal("expected server address")
	}

	ctx := context.Background()
	client := server.Client()
	t.Cleanup(func() {
		_ = client.Close()
	})

	if pong, err := client.Ping(ctx).Result(); err != nil || pong != "PONG" {
		t.Fatalf("expected PING/PONG round trip, got pong=%q err=%v", pong, err)
	}

	server.FailNext("PING", "ERR planned ping failure")
	if err := client.Ping(ctx).Err(); err == nil || !strings.Contains(err.Error(), "planned ping failure") {
		t.Fatalf("expected planned ping failure, got %v", err)
	}

	server.FailNext("GET", "")
	if err := client.Get(ctx, "missing").Err(); err == nil || !strings.Contains(err.Error(), "forced failure") {
		t.Fatalf("expected default forced GET failure, got %v", err)
	}

	if err := client.Set(ctx, "alpha", "one", 0).Err(); err != nil {
		t.Fatalf("expected SET alpha, got %v", err)
	}
	if got, err := client.Get(ctx, "alpha").Result(); err != nil || got != "one" {
		t.Fatalf("expected GET alpha=one, got value=%q err=%v", got, err)
	}
	if got := client.Exists(ctx, "alpha", "missing").Val(); got != 1 {
		t.Fatalf("expected EXISTS count=1, got %d", got)
	}
	if got := client.PExpire(ctx, "alpha", time.Minute).Val(); !got {
		t.Fatalf("expected PEXPIRE existing key to succeed, got %v", got)
	}
	if got := client.PExpire(ctx, "missing", time.Minute).Val(); got {
		t.Fatalf("expected PEXPIRE missing key to report false, got %v", got)
	}

	if added := client.ZAdd(ctx, "scores",
		redis.Z{Score: 2, Member: "two"},
		redis.Z{Score: 1, Member: "one"},
		redis.Z{Score: 3, Member: "three"},
	).Val(); added != 3 {
		t.Fatalf("expected 3 sorted-set inserts, got %d", added)
	}
	if got := client.ZCard(ctx, "scores").Val(); got != 3 {
		t.Fatalf("expected ZCARD=3, got %d", got)
	}
	if got := client.Exists(ctx, "scores").Val(); got != 1 {
		t.Fatalf("expected sorted-set key to participate in EXISTS, got %d", got)
	}
	if members, err := client.ZRange(ctx, "scores", 0, -1).Result(); err != nil || strings.Join(members, ",") != "one,two,three" {
		t.Fatalf("expected ZRANGE ordering, got %v err=%v", members, err)
	}
	rangeByScore := client.ZRangeByScore(ctx, "scores", &redis.ZRangeBy{
		Min:    "1",
		Max:    "3",
		Offset: 1,
		Count:  1,
	})
	if members, err := rangeByScore.Result(); err != nil || len(members) != 1 || members[0] != "two" {
		t.Fatalf("expected ZRANGEBYSCORE LIMIT to return [two], got %v err=%v", members, err)
	}
	if removed := client.ZRem(ctx, "scores", "one", "missing").Val(); removed != 1 {
		t.Fatalf("expected one ZREM deletion, got %d", removed)
	}
	if members, cursor, err := client.Scan(ctx, 0, "sco*", 10).Result(); err != nil || cursor != 0 || len(members) != 1 || members[0] != "scores" {
		t.Fatalf("expected SCAN to return sorted-set key, got members=%v cursor=%d err=%v", members, cursor, err)
	}

	if err := client.Do(ctx, "HELLO", 3).Err(); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("expected HELLO to be rejected, got %v", err)
	}
	if err := client.Do(ctx, "BOGUS").Err(); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("expected unknown command error, got %v", err)
	}

	if deleted := client.Del(ctx, "alpha", "scores", "missing").Val(); deleted != 2 {
		t.Fatalf("expected DEL to remove string and sorted-set keys, got %d", deleted)
	}
}

func TestServerProtocolErrorsAndHelperCodecs(t *testing.T) {
	server := startTestServer(t)

	conn, err := net.Dial("tcp", server.Addr())
	if err != nil {
		t.Fatalf("failed to dial fake redis: %v", err)
	}
	defer conn.Close()

	if _, err := io.WriteString(conn, "*0\r\n"); err != nil {
		t.Fatalf("failed to write empty command: %v", err)
	}
	if line := readRawReplyLine(t, conn); !strings.Contains(line, "ERR empty command") {
		t.Fatalf("expected empty command error, got %q", line)
	}

	conn2, err := net.Dial("tcp", server.Addr())
	if err != nil {
		t.Fatalf("failed to dial fake redis for invalid command: %v", err)
	}
	defer conn2.Close()
	if _, err := io.WriteString(conn2, "PING\r\n"); err != nil {
		t.Fatalf("failed to write invalid RESP payload: %v", err)
	}
	if line := readRawReplyLine(t, conn2); !strings.Contains(line, "ERR invalid command") {
		t.Fatalf("expected invalid command error, got %q", line)
	}

	reader := bufio.NewReader(strings.NewReader("*2\r\n$4\r\nPING\r\n$4\r\nPONG\r\n"))
	command, err := readCommand(reader)
	if err != nil {
		t.Fatalf("expected RESP command to decode, got %v", err)
	}
	if len(command) != 2 || command[0] != "PING" || command[1] != "PONG" {
		t.Fatalf("unexpected command payload: %#v", command)
	}

	if _, err := readCommand(bufio.NewReader(strings.NewReader("PING\r\n"))); err == nil {
		t.Fatal("expected invalid RESP prefix to fail")
	}
	if _, err := readCommand(bufio.NewReader(strings.NewReader("*x\r\n"))); err == nil {
		t.Fatal("expected invalid RESP count to fail")
	}
	if _, err := readCommand(bufio.NewReader(strings.NewReader("*1\r\n!4\r\nPING\r\n"))); err == nil {
		t.Fatal("expected invalid bulk prefix to fail")
	}
	if got, err := readLine(bufio.NewReader(strings.NewReader("hello\r\n"))); err != nil || got != "hello" {
		t.Fatalf("expected readLine to trim CRLF, got %q err=%v", got, err)
	}

	var buf bytes.Buffer
	writer := bufio.NewWriter(&buf)
	writeSimpleString(writer, "OK")
	writeError(writer, "ERR nope")
	writeInteger(writer, 7)
	writeBulkString(writer, "alpha")
	writeNilBulkString(writer)
	writeArrayHeader(writer, 2)
	writeScanReply(writer, "0", []string{"alpha", "scores"})
	if err := writer.Flush(); err != nil {
		t.Fatalf("failed to flush helper writer: %v", err)
	}

	output := buf.String()
	for _, want := range []string{"+OK\r\n", "-ERR nope\r\n", ":7\r\n", "$5\r\nalpha\r\n", "$-1\r\n", "*2\r\n"} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected writer output to contain %q, got %q", want, output)
		}
	}
}

func TestRegisterSessionBindingScriptsAndBindingHelpers(t *testing.T) {
	server := startTestServer(t)

	ctx := context.Background()
	client := server.Client()
	t.Cleanup(func() {
		_ = client.Close()
	})

	hashes := SessionScriptHashes{
		CreateBindingIfAbsent:                "create-binding",
		ReplaceBindingIfSessionMatches:       "replace-binding",
		DeleteBindingIfSessionMatches:        "delete-binding",
		TouchBindingIfSessionMatches:         "touch-binding",
		DeleteBindingAndRevokeIfSessionMatch: "delete-and-revoke",
	}
	server.RegisterSessionBindingScripts(hashes)

	key := "binding:key"
	revocationKey := "binding:revoked"
	bindingA := Binding{
		Key:               key,
		SessionKey:        "session-a",
		Scope:             "chat-realtime",
		SessionID:         "sess-upstream-a",
		CallerNS:          "user:1",
		ChannelID:         11,
		CompatibilityHash: "hash-a",
	}
	bindingB := Binding{
		Key:               key,
		SessionKey:        "session-b",
		Scope:             "chat-realtime",
		SessionID:         "sess-upstream-b",
		CallerNS:          "user:1",
		ChannelID:         22,
		CompatibilityHash: "hash-b",
	}
	rawA := bindingJSON(t, bindingA)
	rawB := bindingJSON(t, bindingB)

	if got := client.EvalSha(ctx, hashes.CreateBindingIfAbsent, []string{key}, rawA).Val(); got != int64(1) {
		t.Fatalf("expected create-if-absent to apply, got %v", got)
	}
	if resolved := server.ResolveBindingPayload(key); resolved == nil || resolved.SessionKey != bindingA.SessionKey {
		t.Fatalf("expected binding payload to resolve, got %#v", resolved)
	}
	if !server.EqualBindingPayload(rawA, &bindingA) {
		t.Fatalf("expected EqualBindingPayload to match identical payload")
	}
	if got := client.EvalSha(ctx, hashes.CreateBindingIfAbsent, []string{key}, rawA).Val(); got != int64(1) {
		t.Fatalf("expected equivalent create-if-absent to be idempotent, got %v", got)
	}
	if got := client.EvalSha(ctx, hashes.CreateBindingIfAbsent, []string{key}, rawB).Val(); got != int64(2) {
		t.Fatalf("expected conflicting create-if-absent to reject, got %v", got)
	}

	if got := client.EvalSha(ctx, hashes.ReplaceBindingIfSessionMatches, []string{key}, rawB, bindingA.SessionKey).Val(); got != int64(1) {
		t.Fatalf("expected matching replace to apply, got %v", got)
	}
	if got := client.EvalSha(ctx, hashes.ReplaceBindingIfSessionMatches, []string{key}, rawA, "wrong-session").Val(); got != int64(2) {
		t.Fatalf("expected mismatched replace to reject, got %v", got)
	}
	if got := client.EvalSha(ctx, hashes.TouchBindingIfSessionMatches, []string{key}, bindingB.SessionKey).Val(); got != int64(1) {
		t.Fatalf("expected matching touch to report success, got %v", got)
	}
	if got := client.EvalSha(ctx, hashes.TouchBindingIfSessionMatches, []string{key}, "missing-session").Val(); got != int64(2) {
		t.Fatalf("expected missing touch to reject, got %v", got)
	}

	if got := client.EvalSha(ctx, hashes.DeleteBindingAndRevokeIfSessionMatch, []string{key, revocationKey}, bindingB.SessionKey).Val(); got != int64(1) {
		t.Fatalf("expected revoke script to delete binding, got %v", got)
	}
	if value, ok := server.GetRaw(revocationKey); !ok || value != "1" {
		t.Fatalf("expected revoke key to be set, got value=%q ok=%v", value, ok)
	}
	if resolved := server.ResolveBindingPayload(key); resolved != nil {
		t.Fatalf("expected revoked binding to be deleted, got %#v", resolved)
	}

	if got := client.EvalSha(ctx, hashes.DeleteBindingIfSessionMatches, []string{"missing"}, bindingA.SessionKey).Val(); got != int64(1) {
		t.Fatalf("expected delete missing binding to be treated as success, got %v", got)
	}
	server.SetRaw(key, rawA)
	if got := client.EvalSha(ctx, hashes.DeleteBindingIfSessionMatches, []string{key}, "wrong-session").Val(); got != int64(2) {
		t.Fatalf("expected mismatched delete to reject, got %v", got)
	}
	if got := client.EvalSha(ctx, hashes.DeleteBindingIfSessionMatches, []string{key}, bindingA.SessionKey).Val(); got != int64(1) {
		t.Fatalf("expected matching delete to apply, got %v", got)
	}

	createHandler := server.handlerForScript("redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])")
	if createHandler == nil || createHandler([]string{key}, []string{rawA, "60000"}) != 1 {
		t.Fatal("expected create handler to be detected and applied")
	}
	replaceHandler := server.handlerForScript("currentBinding and currentBinding.SessionKey == ARGV[2]")
	if replaceHandler == nil || replaceHandler([]string{key}, []string{rawB, bindingA.SessionKey}) != 1 {
		t.Fatal("expected replace handler to be detected and applied")
	}
	revokeHandler := server.handlerForScript("redis.call('SET', KEYS[2], '1', 'PX', ARGV[2])")
	if revokeHandler == nil || revokeHandler([]string{key, revocationKey}, []string{bindingB.SessionKey, "60000"}) != 1 {
		t.Fatal("expected revoke handler to be detected and applied")
	}
	touchHandler := server.handlerForScript("redis.call('PEXPIRE', KEYS[1], ARGV[2])")
	if touchHandler == nil || touchHandler([]string{revocationKey}, []string{"wrong-session", "60000"}) != 2 {
		t.Fatal("expected touch handler mismatch to be reported")
	}
	deleteHandler := server.handlerForScript("redis.call('DEL', KEYS[1])")
	server.SetRaw(key, rawA)
	if deleteHandler == nil || deleteHandler([]string{key}, []string{bindingA.SessionKey}) != 1 {
		t.Fatal("expected delete handler to be detected and applied")
	}
	if server.handlerForScript("return 1") != nil {
		t.Fatal("expected unsupported script to return no handler")
	}

	server.FailNext("SET", "ERR queued one")
	server.FailNext("SET", "")
	if err := client.Set(ctx, "failure", "first", 0).Err(); err == nil || !strings.Contains(err.Error(), "queued one") {
		t.Fatalf("expected first queued failure, got %v", err)
	}
	if err := client.Set(ctx, "failure", "second", 0).Err(); err == nil || !strings.Contains(err.Error(), "forced failure") {
		t.Fatalf("expected second queued failure to use default message, got %v", err)
	}

	if server.ResolveBindingPayload("missing") != nil {
		t.Fatal("expected missing binding lookup to return nil")
	}
	if server.ResolveBindingPayload(revocationKey) != nil {
		t.Fatal("expected non-binding payload lookup to return nil")
	}
	if server.EqualBindingPayload("", &bindingA) {
		t.Fatal("expected empty payload not to match binding")
	}
	if server.EqualBindingPayload(rawA, nil) {
		t.Fatal("expected nil binding not to match payload")
	}
	if decoded := server.decodeBinding(`{"binding":{"Key":"missing-session"}}`); decoded != nil {
		t.Fatalf("expected invalid binding payload to decode to nil, got %#v", decoded)
	}
	if !bindingEquivalent(&bindingA, &bindingA) {
		t.Fatal("expected bindingEquivalent to match identical bindings")
	}
	if bindingEquivalent(&bindingA, &bindingB) {
		t.Fatal("expected bindingEquivalent to reject different bindings")
	}
	if bindingEquivalent(nil, &bindingA) || bindingEquivalent(&bindingA, nil) {
		t.Fatal("expected bindingEquivalent to reject nil bindings")
	}
	if sha1Hex("payload") == "" || sha1Hex("payload") != sha1Hex("payload") {
		t.Fatal("expected deterministic sha1Hex helper")
	}
}
