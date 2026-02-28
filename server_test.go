package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func init() {
	logger = NewLogger("error")
	tracker = NewConnTracker()
	registry = NewTunnelRegistry()
	exposed = NewExposedListeners()
	startTime = time.Now()
}

// ============================================================
// Token helpers tests
// ============================================================

func TestFindToken(t *testing.T) {
	config.Tokens = []TokenConfig{
		{Value: "abc123", Modes: []string{"local-forward"}, Targets: []string{"127.0.0.1:3306"}},
		{Value: "xyz789", Modes: []string{"*"}, Targets: []string{"*"}},
	}

	tok := findToken("abc123")
	if tok == nil || tok.Value != "abc123" {
		t.Fatal("expected to find token abc123")
	}

	tok = findToken("nonexistent")
	if tok != nil {
		t.Fatal("expected nil for nonexistent token")
	}
}

func TestIsModeAllowed(t *testing.T) {
	tok := &TokenConfig{Modes: []string{"local-forward"}}
	if !isModeAllowed(tok, "local-forward") {
		t.Fatal("local-forward should be allowed")
	}
	if isModeAllowed(tok, "remote-forward") {
		t.Fatal("remote-forward should not be allowed")
	}

	tokWild := &TokenConfig{Modes: []string{"*"}}
	if !isModeAllowed(tokWild, "remote-forward") {
		t.Fatal("wildcard should allow any mode")
	}
}

func TestIsTargetAllowed(t *testing.T) {
	tok := &TokenConfig{Targets: []string{"127.0.0.1:3306", "10.0.0.1:80"}}
	if !isTargetAllowed(tok, "127.0.0.1:3306") {
		t.Fatal("127.0.0.1:3306 should be allowed")
	}
	if isTargetAllowed(tok, "192.168.0.1:22") {
		t.Fatal("192.168.0.1:22 should not be allowed")
	}

	tokWild := &TokenConfig{Targets: []string{"*"}}
	if !isTargetAllowed(tokWild, "anything:1234") {
		t.Fatal("wildcard should allow any target")
	}
}

func TestIsTokenExpired(t *testing.T) {
	tok := &TokenConfig{ExpiresAt: ""}
	if isTokenExpired(tok) {
		t.Fatal("empty expiry should not be expired")
	}

	tok = &TokenConfig{ExpiresAt: "2020-01-01T00:00:00Z"}
	if !isTokenExpired(tok) {
		t.Fatal("past date should be expired")
	}

	tok = &TokenConfig{ExpiresAt: "2099-01-01T00:00:00Z"}
	if isTokenExpired(tok) {
		t.Fatal("future date should not be expired")
	}

	tok = &TokenConfig{ExpiresAt: "invalid"}
	if !isTokenExpired(tok) {
		t.Fatal("invalid format should be treated as expired")
	}
}

func TestMaskToken(t *testing.T) {
	if maskToken("abcdef") != "abcd****" {
		t.Fatalf("expected abcd****, got %s", maskToken("abcdef"))
	}
	if maskToken("ab") != "****" {
		t.Fatalf("expected ****, got %s", maskToken("ab"))
	}
}

// ============================================================
// Logger tests
// ============================================================

func TestNewLogger(t *testing.T) {
	l := NewLogger("debug")
	if l.level != LevelDebug {
		t.Fatal("expected debug level")
	}
	l = NewLogger("warn")
	if l.level != LevelWarn {
		t.Fatal("expected warn level")
	}
	l = NewLogger("unknown")
	if l.level != LevelInfo {
		t.Fatal("expected default info level")
	}
}

// ============================================================
// ConnTracker tests
// ============================================================

func TestConnTracker(t *testing.T) {
	ct := NewConnTracker()
	if ct.Count("tok1") != 0 {
		t.Fatal("expected 0")
	}
	ct.Add("tok1")
	ct.Add("tok1")
	if ct.Count("tok1") != 2 {
		t.Fatal("expected 2")
	}
	ct.Remove("tok1")
	if ct.Count("tok1") != 1 {
		t.Fatal("expected 1")
	}
	ct.Remove("tok1")
	if ct.Count("tok1") != 0 {
		t.Fatal("expected 0 after remove all")
	}

	ct.Add("a")
	ct.Add("b")
	all := ct.All()
	if all["a"] != 1 || all["b"] != 1 {
		t.Fatal("expected a=1, b=1")
	}
}

// ============================================================
// TunnelRegistry tests
// ============================================================

func TestTunnelRegistry(t *testing.T) {
	tr := NewTunnelRegistry()
	info := &TunnelInfo{Token: "test", Mode: "local-forward", Target: "127.0.0.1:80"}
	tr.Add("t1", info)
	list := tr.List()
	if len(list) != 1 {
		t.Fatal("expected 1 tunnel")
	}
	tr.Update("t1", 5)
	list = tr.List()
	if list[0].Streams != 5 {
		t.Fatal("expected 5 streams")
	}
	tr.Remove("t1")
	list = tr.List()
	if len(list) != 0 {
		t.Fatal("expected 0 tunnels")
	}
}

// ============================================================
// BandwidthLimiter tests
// ============================================================

func TestBandwidthLimiterUnlimited(t *testing.T) {
	bw := NewBandwidthLimiter(0)
	if !bw.Allow(999999) {
		t.Fatal("unlimited should always allow")
	}
}

func TestBandwidthLimiterLimited(t *testing.T) {
	bw := NewBandwidthLimiter(100)
	if !bw.Allow(50) {
		t.Fatal("should allow 50 of 100")
	}
	if !bw.Allow(50) {
		t.Fatal("should allow another 50")
	}
	if bw.Allow(1) {
		t.Fatal("should reject, limit reached")
	}
}

// ============================================================
// ExposedListeners tests
// ============================================================

func TestExposedListeners(t *testing.T) {
	el := NewExposedListeners()
	ln, _ := net.Listen("tcp", ":0")
	addr := ln.Addr().String()

	if !el.Add(addr, ln) {
		t.Fatal("first add should succeed")
	}
	if el.Add(addr, ln) {
		t.Fatal("duplicate add should fail")
	}
	el.Remove(addr)
	// After remove, should be able to add again (listener closed)
}

// ============================================================
// Health endpoint test
// ============================================================

func TestHealthHandler(t *testing.T) {
	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	healthHandler(w, req)

	resp := w.Result()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var body map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Fatal("expected status ok")
	}
}

// ============================================================
// Status endpoint test
// ============================================================

func TestStatusHandler(t *testing.T) {
	req := httptest.NewRequest("GET", "/status", nil)
	w := httptest.NewRecorder()
	statusHandler(w, req)

	resp := w.Result()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var body map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Fatal("expected status ok")
	}
}

// ============================================================
// WebSocket auth tests (integration)
// ============================================================

func setupTestServer() *httptest.Server {
	config.Tokens = []TokenConfig{
		{
			Value:   "validtoken",
			Modes:   []string{"local-forward"},
			Targets: []string{"127.0.0.1:7777"},
		},
		{
			Value:          "expiredtoken",
			Modes:          []string{"*"},
			Targets:        []string{"*"},
			ExpiresAt:      "2020-01-01T00:00:00Z",
		},
		{
			Value:          "limitedtoken",
			Modes:          []string{"local-forward"},
			Targets:        []string{"127.0.0.1:7777"},
			MaxConnections: 1,
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", wsHandler)
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/status", statusHandler)
	return httptest.NewServer(mux)
}

func wsConnect(t *testing.T, serverURL, msg string) (*websocket.Conn, string) {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(serverURL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("WS dial error: %v", err)
	}
	conn.WriteMessage(websocket.TextMessage, []byte(msg))
	_, resp, err := conn.ReadMessage()
	if err != nil {
		conn.Close()
		t.Fatalf("WS read error: %v", err)
	}
	return conn, string(resp)
}

func TestWSInvalidToken(t *testing.T) {
	s := setupTestServer()
	defer s.Close()

	conn, resp := wsConnect(t, s.URL, "badtoken|local-forward|127.0.0.1:7777")
	defer conn.Close()
	if resp != "ERR|invalid token" {
		t.Fatalf("expected ERR|invalid token, got: %s", resp)
	}
}

func TestWSExpiredToken(t *testing.T) {
	s := setupTestServer()
	defer s.Close()

	conn, resp := wsConnect(t, s.URL, "expiredtoken|local-forward|127.0.0.1:7777")
	defer conn.Close()
	if resp != "ERR|token expired" {
		t.Fatalf("expected ERR|token expired, got: %s", resp)
	}
}

func TestWSModeNotAllowed(t *testing.T) {
	s := setupTestServer()
	defer s.Close()

	conn, resp := wsConnect(t, s.URL, "validtoken|remote-forward|:10000|127.0.0.1:7777")
	defer conn.Close()
	if !strings.HasPrefix(resp, "ERR|mode") {
		t.Fatalf("expected mode error, got: %s", resp)
	}
}

func TestWSTargetNotAllowed(t *testing.T) {
	s := setupTestServer()
	defer s.Close()

	conn, resp := wsConnect(t, s.URL, "validtoken|local-forward|192.168.0.1:22")
	defer conn.Close()
	if !strings.HasPrefix(resp, "ERR|target") {
		t.Fatalf("expected target error, got: %s", resp)
	}
}

func TestWSInvalidFormat(t *testing.T) {
	s := setupTestServer()
	defer s.Close()

	conn, resp := wsConnect(t, s.URL, "onlyonepart")
	defer conn.Close()
	if resp != "ERR|invalid command format" {
		t.Fatalf("expected format error, got: %s", resp)
	}
}

func TestWSLocalForwardSuccess(t *testing.T) {
	// Start echo TCP server
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoLn.Close()
	echoAddr := echoLn.Addr().String()

	go func() {
		for {
			c, err := echoLn.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				n, _ := c.Read(buf)
				c.Write([]byte("ECHO:" + string(buf[:n])))
			}(c)
		}
	}()

	// Update config to allow echo addr
	config.Tokens = []TokenConfig{
		{Value: "tok", Modes: []string{"local-forward"}, Targets: []string{echoAddr}},
	}

	s := httptest.NewServer(http.HandlerFunc(wsHandler))
	defer s.Close()

	wsURL := "ws" + strings.TrimPrefix(s.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	cmd := fmt.Sprintf("tok|local-forward|%s", echoAddr)
	conn.WriteMessage(websocket.TextMessage, []byte(cmd))

	_, msg, _ := conn.ReadMessage()
	if string(msg) != "OK|connected" {
		t.Fatalf("expected OK|connected, got: %s", string(msg))
	}

	// Send data through tunnel
	conn.WriteMessage(websocket.BinaryMessage, []byte("hello"))
	_, resp, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if string(resp) != "ECHO:hello" {
		t.Fatalf("expected ECHO:hello, got: %s", string(resp))
	}
}

func TestWSConnectionLimit(t *testing.T) {
	s := setupTestServer()
	defer s.Close()

	// Start a TCP listener so the first connection can succeed
	echoLn, _ := net.Listen("tcp", "127.0.0.1:7777")
	if echoLn != nil {
		defer echoLn.Close()
		go func() {
			for {
				c, err := echoLn.Accept()
				if err != nil {
					return
				}
				go func(c net.Conn) {
					defer c.Close()
					io.Copy(io.Discard, c)
				}(c)
			}
		}()
	}

	// First connection with limitedtoken (max 1)
	wsURL := "ws" + strings.TrimPrefix(s.URL, "http") + "/ws"
	conn1, _, _ := websocket.DefaultDialer.Dial(wsURL, nil)
	conn1.WriteMessage(websocket.TextMessage, []byte("limitedtoken|local-forward|127.0.0.1:7777"))
	_, msg1, _ := conn1.ReadMessage()

	if string(msg1) != "OK|connected" {
		// If TCP 7777 not available, skip
		conn1.Close()
		t.Skip("TCP 7777 not available, skipping connection limit test")
		return
	}

	// Second connection should be rejected
	conn2, _, _ := websocket.DefaultDialer.Dial(wsURL, nil)
	conn2.WriteMessage(websocket.TextMessage, []byte("limitedtoken|local-forward|127.0.0.1:7777"))
	_, msg2, _ := conn2.ReadMessage()
	conn1.Close()
	conn2.Close()

	if string(msg2) != "ERR|connection limit reached" {
		t.Fatalf("expected connection limit error, got: %s", string(msg2))
	}
}
