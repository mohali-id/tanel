//go:build ignore
// +build ignore

package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
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

type Config struct {
	Remote           string   `yaml:"remote"`
	Token            string   `yaml:"token"`
	Tunnels          []Tunnel `yaml:"tunnels"`
	LogLevel         string   `yaml:"log_level"`
	ReconnectDelay   int      `yaml:"reconnect_delay"`   // seconds, default 5
	ReconnectMaxWait int      `yaml:"reconnect_max_wait"` // seconds, default 60
}

type Tunnel struct {
	Mode   string `yaml:"mode"`
	Local  string `yaml:"local,omitempty"`
	Expose string `yaml:"expose,omitempty"`
	Target string `yaml:"target"`
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
// Multiplexed connection (client side)
// ============================================================

type MuxConn struct {
	WS      *websocket.Conn
	streams map[uint32]net.Conn
	mu      sync.RWMutex
	nextID  uint32
	writeMu sync.Mutex
}

func NewMuxConn(ws *websocket.Conn) *MuxConn {
	return &MuxConn{
		WS:      ws,
		streams: make(map[uint32]net.Conn),
	}
}

func (m *MuxConn) NextStreamID() uint32 {
	return atomic.AddUint32(&m.nextID, 1)
}

func (m *MuxConn) AddStream(id uint32, tcp net.Conn) {
	m.mu.Lock()
	m.streams[id] = tcp
	m.mu.Unlock()
}

func (m *MuxConn) RemoveStream(id uint32) {
	m.mu.Lock()
	if c, ok := m.streams[id]; ok {
		c.Close()
		delete(m.streams, id)
	}
	m.mu.Unlock()
}

func (m *MuxConn) GetStream(id uint32) net.Conn {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.streams[id]
}

func (m *MuxConn) CloseAll() {
	m.mu.Lock()
	for id, c := range m.streams {
		c.Close()
		delete(m.streams, id)
	}
	m.mu.Unlock()
}

func (m *MuxConn) WriteMessage(msgType int, data []byte) error {
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	return m.WS.WriteMessage(msgType, data)
}

func (m *MuxConn) WriteMuxData(streamID uint32, payload []byte) error {
	buf := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(buf[:4], streamID)
	copy(buf[4:], payload)
	return m.WriteMessage(websocket.BinaryMessage, buf)
}

func (m *MuxConn) WriteControl(msg string) error {
	return m.WriteMessage(websocket.TextMessage, []byte(msg))
}

// ============================================================
// CLI flags
// ============================================================

var (
	configPath = flag.String("config", "", "Path to YAML config file")
	remoteFlag = flag.String("remote", "", "Remote WS tunnel URL")
	tokenFlag  = flag.String("token", "", "Auth token")
	modeFlag   = flag.String("mode", "", "Tunnel mode: local-forward or remote-forward")
	localFlag  = flag.String("local", "", "Local listen address (local-forward)")
	exposeFlag = flag.String("expose", "", "Expose address on server (remote-forward)")
	targetFlag = flag.String("target", "", "Target TCP address")
	logLevelFlag = flag.String("log-level", "info", "Log level: debug, info, warn, error")
)

var logger *Logger

// ============================================================
// Main
// ============================================================

func main() {
	flag.Parse()

	var cfg Config

	// Load YAML
	if *configPath != "" {
		file, err := ioutil.ReadFile(*configPath)
		if err != nil {
			log.Fatalf("Read YAML error: %v", err)
		}
		if err := yaml.Unmarshal(file, &cfg); err != nil {
			log.Fatalf("YAML parse error: %v", err)
		}
	}

	// CLI overrides
	if *remoteFlag != "" {
		cfg.Remote = *remoteFlag
	}
	if *tokenFlag != "" {
		cfg.Token = *tokenFlag
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = *logLevelFlag
	}
	if cfg.ReconnectDelay <= 0 {
		cfg.ReconnectDelay = 5
	}
	if cfg.ReconnectMaxWait <= 0 {
		cfg.ReconnectMaxWait = 60
	}

	// Single tunnel from CLI
	if *modeFlag != "" && *targetFlag != "" {
		cfg.Tunnels = append(cfg.Tunnels, Tunnel{
			Mode:   *modeFlag,
			Target: *targetFlag,
			Local:  *localFlag,
			Expose: *exposeFlag,
		})
	}

	// Validate
	if cfg.Remote == "" {
		log.Fatal("Remote URL required. Use --remote or 'remote' in YAML.")
	}
	if cfg.Token == "" {
		log.Fatal("Token required. Use --token or 'token' in YAML.")
	}
	if len(cfg.Tunnels) == 0 {
		log.Fatal("No tunnels defined.")
	}

	logger = NewLogger(cfg.LogLevel)

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	var wg sync.WaitGroup
	done := make(chan struct{})

	go func() {
		<-quit
		logger.Info("Shutting down...")
		close(done)
	}()

	logger.Info("Connecting to %s (%d tunnel(s))", cfg.Remote, len(cfg.Tunnels))

	for _, t := range cfg.Tunnels {
		switch t.Mode {
		case "local-forward":
			wg.Add(1)
			go func(t Tunnel) {
				defer wg.Done()
				runLocalForward(cfg, t, done)
			}(t)
		case "remote-forward":
			wg.Add(1)
			go func(t Tunnel) {
				defer wg.Done()
				runRemoteForward(cfg, t, done)
			}(t)
		default:
			logger.Error("Unknown mode: %s", t.Mode)
		}
	}
	wg.Wait()
	logger.Info("All tunnels stopped.")
}

// ============================================================
// Auto-reconnect wrapper
// ============================================================

func reconnectDelay(attempt int, base, max int) time.Duration {
	delay := time.Duration(base) * time.Second
	for i := 0; i < attempt && delay < time.Duration(max)*time.Second; i++ {
		delay *= 2
	}
	if delay > time.Duration(max)*time.Second {
		delay = time.Duration(max) * time.Second
	}
	return delay
}

// ============================================================
// Local forward with auto-reconnect (simple: 1 WS = 1 TCP)
// ============================================================

func runLocalForward(cfg Config, t Tunnel, done chan struct{}) {
	ln, err := net.Listen("tcp", t.Local)
	if err != nil {
		logger.Error("TCP listen error (%s): %v", t.Local, err)
		return
	}
	defer ln.Close()
	logger.Info("Local forward: %s → %s (via %s)", t.Local, t.Target, cfg.Remote)

	go func() {
		<-done
		ln.Close()
	}()

	for {
		clientConn, err := ln.Accept()
		if err != nil {
			select {
			case <-done:
				return
			default:
				logger.Error("TCP accept error: %v", err)
				continue
			}
		}
		go handleLocalConn(cfg, clientConn, t.Target, done)
	}
}

func handleLocalConn(cfg Config, clientConn net.Conn, targetAddr string, done chan struct{}) {
	defer clientConn.Close()

	attempt := 0
	for {
		select {
		case <-done:
			return
		default:
		}

		wsConn, _, err := websocket.DefaultDialer.Dial(cfg.Remote, nil)
		if err != nil {
			delay := reconnectDelay(attempt, cfg.ReconnectDelay, cfg.ReconnectMaxWait)
			logger.Warn("WS dial error, retry in %v: %v", delay, err)
			attempt++
			time.Sleep(delay)
			continue
		}

		// Send auth + command: token|local-forward|target
		cmd := fmt.Sprintf("%s|local-forward|%s", cfg.Token, targetAddr)
		if err := wsConn.WriteMessage(websocket.TextMessage, []byte(cmd)); err != nil {
			wsConn.Close()
			continue
		}

		// Wait for OK
		_, msg, err := wsConn.ReadMessage()
		if err != nil {
			wsConn.Close()
			continue
		}
		resp := string(msg)
		if strings.HasPrefix(resp, "ERR|") {
			logger.Error("Server rejected: %s", resp)
			wsConn.Close()
			return
		}

		doneCh := make(chan struct{})

		// WS → TCP
		go func() {
			defer close(doneCh)
			for {
				_, data, err := wsConn.ReadMessage()
				if err != nil {
					return
				}
				if _, err := clientConn.Write(data); err != nil {
					return
				}
			}
		}()

		// TCP → WS
		go func() {
			buf := make([]byte, 4096)
			for {
				n, err := clientConn.Read(buf)
				if err != nil {
					wsConn.Close()
					return
				}
				if err := wsConn.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
					wsConn.Close()
					return
				}
			}
		}()

		<-doneCh
		wsConn.Close()
		return
	}
}

// ============================================================
// Remote forward with auto-reconnect + multiplexing
// ============================================================

func runRemoteForward(cfg Config, t Tunnel, done chan struct{}) {
	attempt := 0
	for {
		select {
		case <-done:
			return
		default:
		}

		logger.Info("Remote forward: connecting %s (expose %s → %s)", cfg.Remote, t.Expose, t.Target)
		wsConn, _, err := websocket.DefaultDialer.Dial(cfg.Remote, nil)
		if err != nil {
			delay := reconnectDelay(attempt, cfg.ReconnectDelay, cfg.ReconnectMaxWait)
			logger.Warn("WS dial error, retry in %v: %v", delay, err)
			attempt++
			time.Sleep(delay)
			continue
		}

		mux := NewMuxConn(wsConn)

		// Send auth + command: token|remote-forward|expose|target
		cmd := fmt.Sprintf("%s|remote-forward|%s|%s", cfg.Token, t.Expose, t.Target)
		if err := mux.WriteControl(cmd); err != nil {
			wsConn.Close()
			continue
		}

		// Read messages from server
		reconnect := false
		for {
			msgType, data, err := wsConn.ReadMessage()
			if err != nil {
				logger.Warn("WS read error (remote-forward), reconnecting: %v", err)
				reconnect = true
				break
			}

			if msgType == websocket.TextMessage {
				ctrl := string(data)
				if strings.HasPrefix(ctrl, "ERR|") {
					logger.Error("Server rejected: %s", ctrl)
					wsConn.Close()
					return // don't retry on auth/permission error
				}
				if strings.HasPrefix(ctrl, "OK|") {
					logger.Info("Server: %s", ctrl)
					attempt = 0 // reset on successful connect
					continue
				}
				// open:streamID:target
				if strings.HasPrefix(ctrl, "open:") {
					parts := strings.SplitN(strings.TrimPrefix(ctrl, "open:"), ":", 2)
					if len(parts) >= 2 {
						var sid uint32
						fmt.Sscanf(parts[0], "%d", &sid)
						target := parts[1]
						go handleRemoteStream(mux, sid, target)
					}
					continue
				}
				if strings.HasPrefix(ctrl, "close:") {
					var sid uint32
					fmt.Sscanf(strings.TrimPrefix(ctrl, "close:"), "%d", &sid)
					mux.RemoveStream(sid)
					continue
				}
				logger.Debug("Server: %s", ctrl)
			} else if msgType == websocket.BinaryMessage {
				if len(data) < 4 {
					continue
				}
				sid := binary.BigEndian.Uint32(data[:4])
				payload := data[4:]
				stream := mux.GetStream(sid)
				if stream != nil {
					stream.Write(payload)
				}
			}
		}

		mux.CloseAll()
		wsConn.Close()

		if !reconnect {
			return
		}
		delay := reconnectDelay(attempt, cfg.ReconnectDelay, cfg.ReconnectMaxWait)
		logger.Info("Reconnecting in %v...", delay)
		attempt++
		time.Sleep(delay)
	}
}

func handleRemoteStream(mux *MuxConn, sid uint32, targetAddr string) {
	tcpConn, err := net.Dial("tcp", targetAddr)
	if err != nil {
		logger.Error("TCP connect error (%s): %v", targetAddr, err)
		mux.WriteControl(fmt.Sprintf("close:%d", sid))
		return
	}
	mux.AddStream(sid, tcpConn)
	logger.Debug("Remote stream %d → %s", sid, targetAddr)

	defer func() {
		mux.RemoveStream(sid)
		mux.WriteControl(fmt.Sprintf("close:%d", sid))
	}()

	buf := make([]byte, 4096)
	for {
		n, err := tcpConn.Read(buf)
		if err != nil {
			return
		}
		if err := mux.WriteMuxData(sid, buf[:n]); err != nil {
			return
		}
	}
}
