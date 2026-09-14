package cpa

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRedisQueueClientPopsBatch(t *testing.T) {
	server := newRedisQueueTestServer(t, func(t *testing.T, conn net.Conn) {
		reader := bufio.NewReader(conn)
		if got := readRESPCommand(t, reader); strings.Join(got, " ") != cpaManagementRedisAuthCommand+" secret" {
			t.Fatalf("unexpected auth command: %v", got)
		}
		fmt.Fprint(conn, "+OK\r\n")
		if got := readRESPCommand(t, reader); strings.Join(got, " ") != cpaManagementRedisPopCommand+" "+ManagementUsageQueueKey+" 2" {
			t.Fatalf("unexpected pop command: %v", got)
		}
		fmt.Fprint(conn, "*2\r\n$7\r\n{\"a\":1}\r\n$7\r\n{\"b\":2}\r\n")
	})

	client := NewRedisQueueClient(server.URL, "", "secret", time.Second, ManagementUsageQueueKey, 2)
	messages, err := client.PopUsage(ctxWithTimeout(t))
	if err != nil {
		t.Fatalf("PopUsage returned error: %v", err)
	}

	if len(messages) != 2 || messages[0] != `{"a":1}` || messages[1] != `{"b":2}` {
		t.Fatalf("unexpected messages: %#v", messages)
	}
}

func TestRedisQueueClientFallsBackToHTTPUsageQueueWhenRedisFails(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != cpaManagementUsageQueueEndpoint {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("count"); got != "2" {
			t.Fatalf("expected count=2, got %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Fatalf("expected management Authorization header, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"a":1},{"b":2}]`))
	}))
	defer server.Close()

	client := NewRedisQueueClient(server.URL, "127.0.0.1:1", "secret", 10*time.Millisecond, ManagementUsageQueueKey, 2)
	client.httpClient.httpClient = server.Client()
	messages, err := client.PopUsage(ctxWithTimeout(t))
	if err != nil {
		t.Fatalf("PopUsage returned error: %v", err)
	}
	if len(messages) != 2 || messages[0] != `{"a":1}` || messages[1] != `{"b":2}` {
		t.Fatalf("unexpected messages: %#v", messages)
	}
}

func TestRedisQueueClientPrefersRedisBeforeHTTPFallback(t *testing.T) {
	redisServer := newRedisQueueTestServer(t, func(t *testing.T, conn net.Conn) {
		reader := bufio.NewReader(conn)
		readRESPCommand(t, reader)
		fmt.Fprint(conn, "+OK\r\n")
		readRESPCommand(t, reader)
		fmt.Fprint(conn, "*1\r\n$7\r\n{\"r\":1}\r\n")
	})
	httpCalled := false
	httpServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalled = true
		_, _ = w.Write([]byte(`[{"h":1}]`))
	}))
	defer httpServer.Close()

	client := NewRedisQueueClient(httpServer.URL, redisServer.URL, "secret", time.Second, ManagementUsageQueueKey, 2)
	client.httpClient.httpClient = httpServer.Client()
	messages, err := client.PopUsage(ctxWithTimeout(t))
	if err != nil {
		t.Fatalf("PopUsage returned error: %v", err)
	}
	if httpCalled {
		t.Fatal("expected redis success to skip http fallback")
	}
	if len(messages) != 1 || messages[0] != `{"r":1}` {
		t.Fatalf("unexpected messages: %#v", messages)
	}
}

func TestRedisQueueClientTreatsEmptyPopAsSuccess(t *testing.T) {
	server := newRedisQueueTestServer(t, func(t *testing.T, conn net.Conn) {
		reader := bufio.NewReader(conn)
		readRESPCommand(t, reader)
		fmt.Fprint(conn, "+OK\r\n")
		readRESPCommand(t, reader)
		fmt.Fprint(conn, "*0\r\n")
	})

	client := NewRedisQueueClient(server.URL, "", "secret", time.Second, ManagementUsageQueueKey, 1000)
	messages, err := client.PopUsage(ctxWithTimeout(t))
	if err != nil {
		t.Fatalf("PopUsage returned error: %v", err)
	}
	if len(messages) != 0 {
		t.Fatalf("expected empty messages, got %#v", messages)
	}
}

func TestRedisQueueClientClassifiesAuthErrors(t *testing.T) {
	server := newRedisQueueTestServer(t, func(t *testing.T, conn net.Conn) {
		readRESPCommand(t, bufio.NewReader(conn))
		fmt.Fprint(conn, "-ERR invalid password\r\n")
	})

	client := NewRedisQueueClient(server.URL, "", "wrong", time.Second, ManagementUsageQueueKey, 1000)
	_, err := client.PopUsage(ctxWithTimeout(t))
	if err == nil {
		t.Fatal("expected auth error")
	}
	if !errors.Is(err, ErrRedisQueueAuth) {
		t.Fatalf("expected ErrRedisQueueAuth, got %v", err)
	}
}

func TestRedisQueueClientPrefersExplicitQueueAddr(t *testing.T) {
	if got := redisQueueAddress("https://cpa.example.com", "redis-stream.example.com:6380"); got != "redis-stream.example.com:6380" {
		t.Fatalf("expected explicit redis queue addr, got %q", got)
	}
	if got := redisQueueAddress("https://cpa.example.com", "redis://redis-stream.example.com:6380"); got != "redis-stream.example.com:6380" {
		t.Fatalf("expected redis scheme to be stripped, got %q", got)
	}
	if got := redisQueueAddress("https://cpa.example.com", "http://redis-stream.example.com:6380"); got != "redis-stream.example.com:6380" {
		t.Fatalf("expected http scheme to be stripped, got %q", got)
	}
}

func TestRedisQueueClientDefaultsToManagementPortFromBaseURLHost(t *testing.T) {
	if got := redisQueueAddress("https://cpa.example.com", ""); got != "cpa.example.com:"+ManagementRedisDefaultPort {
		t.Fatalf("expected default management port from https host, got %q", got)
	}
	if got := redisQueueAddress("http://cpa.example.com", ""); got != "cpa.example.com:"+ManagementRedisDefaultPort {
		t.Fatalf("expected default management port from http host, got %q", got)
	}
	if got := redisQueueAddress("http://127.0.0.1:"+ManagementRedisDefaultPort, ""); got != "127.0.0.1:"+ManagementRedisDefaultPort {
		t.Fatalf("expected explicit port to be preserved, got %q", got)
	}
}

func TestRedisQueueClientReportsMalformedRESP(t *testing.T) {
	server := newRedisQueueTestServer(t, func(t *testing.T, conn net.Conn) {
		reader := bufio.NewReader(conn)
		readRESPCommand(t, reader)
		fmt.Fprint(conn, "+OK\r\n")
		readRESPCommand(t, reader)
		fmt.Fprint(conn, "!not-resp\r\n")
	})

	client := NewRedisQueueClient(server.URL, "", "secret", time.Second, ManagementUsageQueueKey, 1000)
	_, err := client.PopUsage(ctxWithTimeout(t))
	if err == nil || !strings.Contains(err.Error(), "read redis queue pop response") {
		t.Fatalf("expected malformed response error, got %v", err)
	}
}

type redisQueueTestServer struct {
	URL string
}

func newRedisQueueTestServer(t *testing.T, handler func(*testing.T, net.Conn)) redisQueueTestServer {
	t.Helper()
	listener, err := net.Listen(cpaManagementRedisNetwork, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		handler(t, conn)
	}()
	t.Cleanup(func() { <-done })

	return redisQueueTestServer{URL: "http://" + listener.Addr().String()}
}

func readRESPCommand(t *testing.T, reader *bufio.Reader) []string {
	t.Helper()
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read command header: %v", err)
	}
	var count int
	if _, err := fmt.Sscanf(line, "*%d\r\n", &count); err != nil {
		t.Fatalf("parse command header %q: %v", line, err)
	}
	parts := make([]string, 0, count)
	for range count {
		bulkHeader, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read bulk header: %v", err)
		}
		var size int
		if _, err := fmt.Sscanf(bulkHeader, "$%d\r\n", &size); err != nil {
			t.Fatalf("parse bulk header %q: %v", bulkHeader, err)
		}
		buf := make([]byte, size+2)
		if _, err := reader.Read(buf); err != nil {
			t.Fatalf("read bulk body: %v", err)
		}
		parts = append(parts, string(buf[:size]))
	}
	return parts
}

func ctxWithTimeout(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestRedisQueueClientWithShardsDefaults(t *testing.T) {
	c1 := NewRedisQueueClient("http://cpa.example.com", "", "secret", time.Second, ManagementUsageQueueKey, 100)
	if c1.shards != 1 {
		t.Fatalf("expected NewRedisQueueClient to have shards=1, got %d", c1.shards)
	}

	c2 := NewRedisQueueClientWithShards("http://cpa.example.com", "", "secret", time.Second, ManagementUsageQueueKey, 100, 0)
	if c2.shards != 1 {
		t.Fatalf("expected non-positive shards to normalize to 1, got %d", c2.shards)
	}

	c3 := NewRedisQueueClientWithShards("http://cpa.example.com", "", "secret", time.Second, ManagementUsageQueueKey, 100, -3)
	if c3.shards != 1 {
		t.Fatalf("expected negative shards to normalize to 1, got %d", c3.shards)
	}

	c4 := NewRedisQueueClientWithShards("http://cpa.example.com", "", "secret", time.Second, ManagementUsageQueueKey, 100, 16)
	if c4.shards != 16 {
		t.Fatalf("expected explicit shards 16, got %d", c4.shards)
	}
}

func TestRedisQueueClientHTTPFallbackWithShardsRotatesAndReturnsImmediatelyOnHit(t *testing.T) {
	var requestedHeaders []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("X-Forwarded-For")
		requestedHeaders = append(requestedHeaders, h)
		w.Header().Set("Content-Type", "application/json")
		switch h {
		case "10.0.0.1":
			_, _ = w.Write([]byte(`[]`))
		case "10.0.0.2":
			_, _ = w.Write([]byte(`[{"hit":"shard-2"}]`))
		case "10.0.0.3":
			_, _ = w.Write([]byte(`[{"hit":"shard-3"}]`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer server.Close()

	client := NewRedisQueueClientWithShards(server.URL, "127.0.0.1:1", "secret", 10*time.Millisecond, ManagementUsageQueueKey, 2, 4)
	client.httpClient.httpClient = server.Client()

	// First call: shard 1 is empty, shard 2 hits -> returns immediately without querying shard 3 or 4
	messages, err := client.PopUsage(ctxWithTimeout(t))
	if err != nil {
		t.Fatalf("first PopUsage error: %v", err)
	}
	if len(messages) != 1 || messages[0] != `{"hit":"shard-2"}` {
		t.Fatalf("unexpected first messages: %#v", messages)
	}
	if len(requestedHeaders) != 2 || requestedHeaders[0] != "10.0.0.1" || requestedHeaders[1] != "10.0.0.2" {
		t.Fatalf("expected headers [10.0.0.1 10.0.0.2], got %#v", requestedHeaders)
	}

	// Second call: should start probing at shard 3 (10.0.0.3)
	requestedHeaders = nil
	messages, err = client.PopUsage(ctxWithTimeout(t))
	if err != nil {
		t.Fatalf("second PopUsage error: %v", err)
	}
	if len(messages) != 1 || messages[0] != `{"hit":"shard-3"}` {
		t.Fatalf("unexpected second messages: %#v", messages)
	}
	if len(requestedHeaders) != 1 || requestedHeaders[0] != "10.0.0.3" {
		t.Fatalf("expected second headers [10.0.0.3], got %#v", requestedHeaders)
	}
}

func TestRedisQueueClientHTTPFallbackAllShardsEmptyQueriesAllShards(t *testing.T) {
	var requestedHeaders []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedHeaders = append(requestedHeaders, r.Header.Get("X-Forwarded-For"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()

	client := NewRedisQueueClientWithShards(server.URL, "127.0.0.1:1", "secret", 10*time.Millisecond, ManagementUsageQueueKey, 2, 3)
	client.httpClient.httpClient = server.Client()

	messages, err := client.PopUsage(ctxWithTimeout(t))
	if err != nil {
		t.Fatalf("PopUsage error: %v", err)
	}
	if len(messages) != 0 {
		t.Fatalf("expected empty messages, got %#v", messages)
	}
	if len(requestedHeaders) != 3 || requestedHeaders[0] != "10.0.0.1" || requestedHeaders[1] != "10.0.0.2" || requestedHeaders[2] != "10.0.0.3" {
		t.Fatalf("expected all 3 shards queried [10.0.0.1 10.0.0.2 10.0.0.3], got %#v", requestedHeaders)
	}
}

func TestRedisQueueClientHTTPFallbackSingleShardOmitsForwardedFor(t *testing.T) {
	var requestedHeaders []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedHeaders = append(requestedHeaders, r.Header.Get("X-Forwarded-For"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"single":true}]`))
	}))
	defer server.Close()

	client := NewRedisQueueClientWithShards(server.URL, "127.0.0.1:1", "secret", 10*time.Millisecond, ManagementUsageQueueKey, 2, 1)
	client.httpClient.httpClient = server.Client()

	messages, err := client.PopUsage(ctxWithTimeout(t))
	if err != nil {
		t.Fatalf("PopUsage error: %v", err)
	}
	if len(messages) != 1 || messages[0] != `{"single":true}` {
		t.Fatalf("unexpected messages: %#v", messages)
	}
	if len(requestedHeaders) != 1 || requestedHeaders[0] != "" {
		t.Fatalf("expected empty X-Forwarded-For header, got %#v", requestedHeaders)
	}
}
