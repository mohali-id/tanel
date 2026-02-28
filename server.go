package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"gopkg.in/yaml.v3"
)

// ============================================================
// Config
// ============================================================

type ServerConfig struct {
	Listen   string        `yaml:"listen"`
	LogLevel string        `yaml:"log_level"` // debug, info, warn, error
	Prefix   string        `yaml:"prefix"`    // endpoint prefix, e.g. "socket" → /socket/ws
	Tokens   []TokenConfig `yaml:"tokens"`
}

type TokenConfig struct {
	Value           string   `yaml:"value"`
	Modes           []string `yaml:"modes"`
	Targets         []string `yaml:"targets"`
	MaxConnections  int      `yaml:"max_connections,omitempty"`  // 0 = unlimited
	BandwidthLimit  int64    `yaml:"bandwidth_limit,omitempty"` // bytes/sec, 0 = unlimited
	ExpiresAt       string   `yaml:"expires_at,omitempty"`      // RFC3339
}

// ============================================================
// Logger
// ============================================================

const (
	LevelDebug = iota
	LevelInfo
	LevelWarn
	LevelError
)

type Logger struct {
	level int
}

func NewLogger(level string) *Logger {
	l := &Logger{level: LevelInfo}
	switch strings.ToLower(level) {
	case "debug":
		l.level = LevelDebug
	case "info":
		l.level = LevelInfo
	case "warn":
		l.level = LevelWarn
	case "error":
		l.level = LevelError
	}
	return l
}

func (l *Logger) Debug(format string, v ...interface{}) {
	if l.level <= LevelDebug {
		log.Printf("[DEBUG] "+format, v...)
	}
}
func (l *Logger) Info(format string, v ...interface{}) {
	if l.level <= LevelInfo {
		log.Printf("[INFO] "+format, v...)
	}
}
func (l *Logger) Warn(format string, v ...interface{}) {
	if l.level <= LevelWarn {
		log.Printf("[WARN] "+format, v...)
	}
}
func (l *Logger) Error(format string, v ...interface{}) {
	if l.level <= LevelError {
		log.Printf("[ERROR] "+format, v...)
	}
}

// ============================================================
// Multiplexed stream
// ============================================================

type Stream struct {
	ID      uint32
	TCPConn net.Conn
}

type MuxConn struct {
	WS       *websocket.Conn
	streams  map[uint32]*Stream
	mu       sync.RWMutex
	nextID   uint32
	writeMu  sync.Mutex
}

func NewMuxConn(ws *websocket.Conn) *MuxConn {
	return &MuxConn{
		WS:      ws,
		streams: make(map[uint32]*Stream),
	}
}

func (m *MuxConn) NextStreamID() uint32 {
	return atomic.AddUint32(&m.nextID, 1)
}

func (m *MuxConn) AddStream(id uint32, tcp net.Conn) {
	m.mu.Lock()
	m.streams[id] = &Stream{ID: id, TCPConn: tcp}
	m.mu.Unlock()
}

func (m *MuxConn) RemoveStream(id uint32) {
	m.mu.Lock()
	if s, ok := m.streams[id]; ok {
		s.TCPConn.Close()
		delete(m.streams, id)
	}
	m.mu.Unlock()
}

func (m *MuxConn) GetStream(id uint32) *Stream {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.streams[id]
}

func (m *MuxConn) StreamCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.streams)
}

func (m *MuxConn) CloseAll() {
	m.mu.Lock()
	for id, s := range m.streams {
		s.TCPConn.Close()
		delete(m.streams, id)
	}
	m.mu.Unlock()
}

// WriteMessage with mutex for concurrent safety
func (m *MuxConn) WriteMessage(msgType int, data []byte) error {
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	return m.WS.WriteMessage(msgType, data)
}

// WriteMuxData writes multiplexed binary data: [4-byte streamID][payload]
func (m *MuxConn) WriteMuxData(streamID uint32, payload []byte) error {
	buf := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(buf[:4], streamID)
	copy(buf[4:], payload)
	return m.WriteMessage(websocket.BinaryMessage, buf)
}

// WriteControl writes a text control message
func (m *MuxConn) WriteControl(msg string) error {
	return m.WriteMessage(websocket.TextMessage, []byte(msg))
}

// ============================================================
// Bandwidth limiter
// ============================================================

type BandwidthLimiter struct {
	limit     int64 // bytes per second, 0 = unlimited
	used      int64
	mu        sync.Mutex
	resetTime time.Time
}

func NewBandwidthLimiter(bytesPerSec int64) *BandwidthLimiter {
	return &BandwidthLimiter{
		limit:     bytesPerSec,
		resetTime: time.Now(),
	}
}

func (b *BandwidthLimiter) Allow(n int) bool {
	if b.limit <= 0 {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if now.Sub(b.resetTime) >= time.Second {
		b.used = 0
		b.resetTime = now
	}
	if b.used+int64(n) > b.limit {
		return false
	}
	b.used += int64(n)
	return true
}

func (b *BandwidthLimiter) WaitAndAllow(n int) {
	for !b.Allow(n) {
		time.Sleep(10 * time.Millisecond)
	}
}

// ============================================================
// Connection tracker (per token)
// ============================================================

type ConnTracker struct {
	counts map[string]int
	mu     sync.Mutex
}

func NewConnTracker() *ConnTracker {
	return &ConnTracker{counts: make(map[string]int)}
}

func (ct *ConnTracker) Add(token string) int {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	ct.counts[token]++
	return ct.counts[token]
}

func (ct *ConnTracker) Remove(token string) {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	ct.counts[token]--
	if ct.counts[token] <= 0 {
		delete(ct.counts, token)
	}
}

func (ct *ConnTracker) Count(token string) int {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	return ct.counts[token]
}

func (ct *ConnTracker) All() map[string]int {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	result := make(map[string]int)
	for k, v := range ct.counts {
		result[k] = v
	}
	return result
}

// ============================================================
// Active tunnel info (for dashboard)
// ============================================================

type TunnelInfo struct {
	Token     string    `json:"token"`
	Mode      string    `json:"mode"`
	Target    string    `json:"target"`
	Expose    string    `json:"expose,omitempty"`
	Streams   int       `json:"streams"`
	StartedAt time.Time `json:"started_at"`
	RemoteAddr string   `json:"remote_addr"`
}

type TunnelRegistry struct {
	tunnels map[string]*TunnelInfo // keyed by unique id
	mu      sync.RWMutex
}

func NewTunnelRegistry() *TunnelRegistry {
	return &TunnelRegistry{tunnels: make(map[string]*TunnelInfo)}
}

func (tr *TunnelRegistry) Add(id string, info *TunnelInfo) {
	tr.mu.Lock()
	tr.tunnels[id] = info
	tr.mu.Unlock()
}

func (tr *TunnelRegistry) Remove(id string) {
	tr.mu.Lock()
	delete(tr.tunnels, id)
	tr.mu.Unlock()
}

func (tr *TunnelRegistry) Update(id string, streams int) {
	tr.mu.Lock()
	if t, ok := tr.tunnels[id]; ok {
		t.Streams = streams
	}
	tr.mu.Unlock()
}

func (tr *TunnelRegistry) List() []*TunnelInfo {
	tr.mu.RLock()
	defer tr.mu.RUnlock()
	var list []*TunnelInfo
	for _, t := range tr.tunnels {
		list = append(list, t)
	}
	return list
}

// ============================================================
// Exposed listeners (remote forward)
// ============================================================

type ExposedListeners struct {
	listeners map[string]net.Listener
	mu        sync.Mutex
}

func NewExposedListeners() *ExposedListeners {
	return &ExposedListeners{listeners: make(map[string]net.Listener)}
}

func (e *ExposedListeners) Add(addr string, ln net.Listener) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.listeners[addr]; ok {
		return false
	}
	e.listeners[addr] = ln
	return true
}

func (e *ExposedListeners) Remove(addr string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if ln, ok := e.listeners[addr]; ok {
		ln.Close()
		delete(e.listeners, addr)
	}
}

func (e *ExposedListeners) CloseAll() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for addr, ln := range e.listeners {
		ln.Close()
		delete(e.listeners, addr)
	}
}

// ============================================================
// Globals
// ============================================================

var (
	listenAddr = flag.String("listen", ":9000", "WebSocket listen address")
	configPath = flag.String("config", "", "Path to server config YAML")
	tokenFlag  = flag.String("token", "", "Single token via CLI (full access)")
	logLevel   = flag.String("log-level", "info", "Log level: debug, info, warn, error")
	prefixFlag = flag.String("prefix", "", "Endpoint prefix, e.g. 'socket' → /socket/ws")
)

var (
	config   ServerConfig
	logger   *Logger
	tracker  *ConnTracker
	registry *TunnelRegistry
	exposed  *ExposedListeners
	startTime time.Time
)

// ============================================================
// Token helpers
// ============================================================

func findToken(value string) *TokenConfig {
	for _, t := range config.Tokens {
		if t.Value == value {
			return &t
		}
	}
	return nil
}

func isModeAllowed(token *TokenConfig, mode string) bool {
	for _, m := range token.Modes {
		if m == mode || m == "*" {
			return true
		}
	}
	return false
}

func isTargetAllowed(token *TokenConfig, target string) bool {
	for _, t := range token.Targets {
		if t == "*" || t == target {
			return true
		}
	}
	return false
}

func isTokenExpired(token *TokenConfig) bool {
	if token.ExpiresAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, token.ExpiresAt)
	if err != nil {
		return true // invalid format = expired
	}
	return time.Now().After(t)
}

func maskToken(token string) string {
	if len(token) <= 4 {
		return "****"
	}
	return token[:4] + "****"
}

// ============================================================
// Main
// ============================================================

func main() {
	flag.Parse()
	startTime = time.Now()

	// Load config
	if *configPath != "" {
		file, err := ioutil.ReadFile(*configPath)
		if err != nil {
			log.Fatalf("Read config error: %v", err)
		}
		if err := yaml.Unmarshal(file, &config); err != nil {
			log.Fatalf("YAML parse error: %v", err)
		}
		if config.Listen != "" {
			*listenAddr = config.Listen
		}
		if config.LogLevel != "" {
			*logLevel = config.LogLevel
		}
	}

	// CLI token
	if *tokenFlag != "" {
		config.Tokens = append(config.Tokens, TokenConfig{
			Value:   *tokenFlag,
			Modes:   []string{"*"},
			Targets: []string{"*"},
		})
	}

	if len(config.Tokens) == 0 {
		log.Fatal("No tokens configured. Use --config or --token.")
	}

	logger = NewLogger(*logLevel)
	tracker = NewConnTracker()
	registry = NewTunnelRegistry()
	exposed = NewExposedListeners()

	// Prefix
	if *prefixFlag != "" {
		config.Prefix = *prefixFlag
	}
	prefix := ""
	if config.Prefix != "" {
		prefix = "/" + strings.Trim(config.Prefix, "/")
	}

	// HTTP handlers
	mux := http.NewServeMux()
	mux.HandleFunc(prefix+"/ws", wsHandler)
	mux.HandleFunc(prefix+"/health", healthHandler)
	mux.HandleFunc(prefix+"/status", statusHandler)

	server := &http.Server{
		Addr:    *listenAddr,
		Handler: mux,
	}

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-quit
		logger.Info("Shutting down server...")
		exposed.CloseAll()
		server.Close()
	}()

	if prefix != "" {
		logger.Info("Server listening on %s (prefix: %s, %d token(s))", *listenAddr, prefix, len(config.Tokens))
	} else {
		logger.Info("Server listening on %s (%d token(s) registered)", *listenAddr, len(config.Tokens))
	}
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
	logger.Info("Server stopped.")
}

// ============================================================
// Health check
// ============================================================

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "ok",
		"uptime": time.Since(startTime).String(),
	})
}

// ============================================================
// Status dashboard
// ============================================================

func statusHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":      "ok",
		"uptime":      time.Since(startTime).String(),
		"started_at":  startTime.Format(time.RFC3339),
		"tokens":      len(config.Tokens),
		"connections": tracker.All(),
		"tunnels":     registry.List(),
	})
}

// ============================================================
// WebSocket handler
// ============================================================

func wsHandler(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		logger.Error("WS upgrade error: %v", err)
		return
	}
	defer conn.Close()

	// Read auth + command: token|mode|... 
	_, msg, err := conn.ReadMessage()
	if err != nil {
		logger.Error("WS read error: %v", err)
		return
	}

	parts := strings.Split(string(msg), "|")
	if len(parts) < 3 {
		conn.WriteMessage(websocket.TextMessage, []byte("ERR|invalid command format"))
		return
	}

	tokenValue := parts[0]
	mode := parts[1]

	// Validate token
	token := findToken(tokenValue)
	if token == nil {
		logger.Warn("Auth failed: invalid token %s from %s", maskToken(tokenValue), r.RemoteAddr)
		conn.WriteMessage(websocket.TextMessage, []byte("ERR|invalid token"))
		return
	}

	// Check expiry
	if isTokenExpired(token) {
		logger.Warn("Auth failed: expired token %s from %s", maskToken(tokenValue), r.RemoteAddr)
		conn.WriteMessage(websocket.TextMessage, []byte("ERR|token expired"))
		return
	}

	// Check mode
	if !isModeAllowed(token, mode) {
		logger.Warn("Auth failed: mode '%s' not allowed for %s from %s", mode, maskToken(tokenValue), r.RemoteAddr)
		conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("ERR|mode '%s' not allowed", mode)))
		return
	}

	// Check connection limit
	if token.MaxConnections > 0 && tracker.Count(tokenValue) >= token.MaxConnections {
		logger.Warn("Connection limit reached for %s from %s", maskToken(tokenValue), r.RemoteAddr)
		conn.WriteMessage(websocket.TextMessage, []byte("ERR|connection limit reached"))
		return
	}

	tracker.Add(tokenValue)
	defer tracker.Remove(tokenValue)

	// Bandwidth limiter
	bwLimiter := NewBandwidthLimiter(token.BandwidthLimit)

	switch mode {
	case "local-forward":
		handleLocalForward(conn, token, parts, r.RemoteAddr, tokenValue, bwLimiter)
	case "remote-forward":
		handleRemoteForward(conn, token, parts, r.RemoteAddr, tokenValue, bwLimiter)
	default:
		conn.WriteMessage(websocket.TextMessage, []byte("ERR|unknown mode"))
	}
}

// ============================================================
// Local forward (simple: 1 WS = 1 TCP)
// ============================================================

func handleLocalForward(conn *websocket.Conn, token *TokenConfig, parts []string, remoteAddr, tokenValue string, bw *BandwidthLimiter) {
	if len(parts) != 3 {
		conn.WriteMessage(websocket.TextMessage, []byte("ERR|invalid local-forward format"))
		return
	}
	targetAddr := parts[2]

	if !isTargetAllowed(token, targetAddr) {
		logger.Warn("Target '%s' not allowed for %s from %s", targetAddr, maskToken(tokenValue), remoteAddr)
		conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("ERR|target '%s' not allowed", targetAddr)))
		return
	}

	tunnelID := fmt.Sprintf("lf-%s-%s-%d", remoteAddr, targetAddr, time.Now().UnixNano())
	info := &TunnelInfo{
		Token:      maskToken(tokenValue),
		Mode:       "local-forward",
		Target:     targetAddr,
		Streams:    1,
		StartedAt:  time.Now(),
		RemoteAddr: remoteAddr,
	}
	registry.Add(tunnelID, info)
	defer registry.Remove(tunnelID)

	tcpConn, err := net.Dial("tcp", targetAddr)
	if err != nil {
		logger.Error("TCP connect error (%s): %v", targetAddr, err)
		conn.WriteMessage(websocket.TextMessage, []byte("ERR|tcp connect failed: "+err.Error()))
		return
	}
	defer tcpConn.Close()

	logger.Info("Local forward: %s → %s (token: %s)", remoteAddr, targetAddr, maskToken(tokenValue))
	conn.WriteMessage(websocket.TextMessage, []byte("OK|connected"))

	// TCP → WS
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := tcpConn.Read(buf)
			if err != nil {
				return
			}
			bw.WaitAndAllow(n)
			if err := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
				return
			}
		}
	}()

	// WS → TCP
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			logger.Debug("WS read error (local-forward): %v", err)
			return
		}
		bw.WaitAndAllow(len(data))
		if _, err := tcpConn.Write(data); err != nil {
			return
		}
	}
}

// ============================================================
// Remote forward (with multiplexing)
// ============================================================

func handleRemoteForward(conn *websocket.Conn, token *TokenConfig, parts []string, remoteAddr, tokenValue string, bw *BandwidthLimiter) {
	if len(parts) != 4 {
		conn.WriteMessage(websocket.TextMessage, []byte("ERR|invalid remote-forward format"))
		return
	}
	exposeAddr := parts[2]
	clientTarget := parts[3]

	if !isTargetAllowed(token, clientTarget) {
		logger.Warn("Target '%s' not allowed for %s from %s", clientTarget, maskToken(tokenValue), remoteAddr)
		conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("ERR|target '%s' not allowed", clientTarget)))
		return
	}

	ln, err := net.Listen("tcp", exposeAddr)
	if err != nil {
		conn.WriteMessage(websocket.TextMessage, []byte("ERR|expose failed: "+err.Error()))
		return
	}
	if !exposed.Add(exposeAddr, ln) {
		ln.Close()
		conn.WriteMessage(websocket.TextMessage, []byte("ERR|port already exposed"))
		return
	}

	mux := NewMuxConn(conn)
	tunnelID := fmt.Sprintf("rf-%s-%s-%d", exposeAddr, clientTarget, time.Now().UnixNano())
	info := &TunnelInfo{
		Token:      maskToken(tokenValue),
		Mode:       "remote-forward",
		Target:     clientTarget,
		Expose:     exposeAddr,
		StartedAt:  time.Now(),
		RemoteAddr: remoteAddr,
	}
	registry.Add(tunnelID, info)

	logger.Info("Remote forward: expose %s → client %s (token: %s)", exposeAddr, clientTarget, maskToken(tokenValue))
	mux.WriteControl("OK|exposed " + exposeAddr)

	defer func() {
		exposed.Remove(exposeAddr)
		mux.CloseAll()
		registry.Remove(tunnelID)
		logger.Info("Remote forward closed: %s", exposeAddr)
	}()

	// Accept incoming TCP on exposed port → relay to client via WS
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			sid := mux.NextStreamID()
			mux.AddStream(sid, c)
			registry.Update(tunnelID, mux.StreamCount())
			mux.WriteControl(fmt.Sprintf("open:%d:%s", sid, clientTarget))
			logger.Debug("Remote stream %d opened on %s", sid, exposeAddr)

			// TCP → WS relay
			go func(sid uint32, tcp net.Conn) {
				defer func() {
					mux.RemoveStream(sid)
					registry.Update(tunnelID, mux.StreamCount())
					mux.WriteControl(fmt.Sprintf("close:%d", sid))
				}()
				buf := make([]byte, 4096)
				for {
					n, err := tcp.Read(buf)
					if err != nil {
						return
					}
					bw.WaitAndAllow(n)
					if err := mux.WriteMuxData(sid, buf[:n]); err != nil {
						return
					}
				}
			}(sid, c)
		}
	}()

	// Read from WS: binary data or control
	for {
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			logger.Debug("WS read error (remote-forward): %v", err)
			return
		}

		if msgType == websocket.TextMessage {
			ctrl := string(data)
			if strings.HasPrefix(ctrl, "close:") {
				var sid uint32
				fmt.Sscanf(strings.TrimPrefix(ctrl, "close:"), "%d", &sid)
				mux.RemoveStream(sid)
				registry.Update(tunnelID, mux.StreamCount())
			}
		} else if msgType == websocket.BinaryMessage {
			if len(data) < 4 {
				continue
			}
			sid := binary.BigEndian.Uint32(data[:4])
			payload := data[4:]
			stream := mux.GetStream(sid)
			if stream != nil {
				bw.WaitAndAllow(len(payload))
				stream.TCPConn.Write(payload)
			}
		}
	}
}
