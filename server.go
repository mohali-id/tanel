package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
	"gopkg.in/yaml.v3"
)

// --- Config structures ---

type ServerConfig struct {
	Listen string        `yaml:"listen"`
	Tokens []TokenConfig `yaml:"tokens"`
}

type TokenConfig struct {
	Value   string   `yaml:"value"`
	Modes   []string `yaml:"modes"`
	Targets []string `yaml:"targets"`
}

// --- Globals ---

var (
	listenAddr = flag.String("listen", ":9000", "WebSocket listen address")
	configPath = flag.String("config", "", "Path to server config YAML")
	tokenFlag  = flag.String("token", "", "Single token (CLI mode, allows all modes/targets)")
)

var config ServerConfig

var exposed = struct {
	listeners map[string]net.Listener
	sync.Mutex
}{listeners: map[string]net.Listener{}}

// --- Token lookup & validation ---

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

// --- Main ---

func main() {
	flag.Parse()

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
	}

	// CLI --token: single token, all modes, all targets
	if *tokenFlag != "" {
		config.Tokens = append(config.Tokens, TokenConfig{
			Value:   *tokenFlag,
			Modes:   []string{"*"},
			Targets: []string{"*"},
		})
	}

	if len(config.Tokens) == 0 {
		log.Fatal("No tokens configured. Use --config or --token to set at least one token.")
	}

	log.Printf("Server listening on %s (%d token(s) registered)", *listenAddr, len(config.Tokens))
	http.HandleFunc("/ws", wsHandler)
	log.Fatal(http.ListenAndServe(*listenAddr, nil))
}

// --- WebSocket handler ---

func wsHandler(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("WS upgrade error:", err)
		return
	}
	defer conn.Close()

	// First message: token|mode|target or token|mode|expose|target
	_, msg, err := conn.ReadMessage()
	if err != nil {
		log.Println("WS read error:", err)
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
		log.Printf("Auth failed: invalid token from %s", r.RemoteAddr)
		conn.WriteMessage(websocket.TextMessage, []byte("ERR|invalid token"))
		return
	}

	// Validate mode
	if !isModeAllowed(token, mode) {
		log.Printf("Auth failed: mode '%s' not allowed for token from %s", mode, r.RemoteAddr)
		conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("ERR|mode '%s' not allowed", mode)))
		return
	}

	switch mode {
	case "local-forward":
		handleLocalForward(conn, token, parts, r.RemoteAddr)
	case "remote-forward":
		handleRemoteForward(conn, token, parts, r.RemoteAddr)
	default:
		conn.WriteMessage(websocket.TextMessage, []byte("ERR|unknown mode"))
	}
}

// --- Local forward handler ---

func handleLocalForward(conn *websocket.Conn, token *TokenConfig, parts []string, remoteAddr string) {
	// Format: token|local-forward|target
	if len(parts) != 3 {
		conn.WriteMessage(websocket.TextMessage, []byte("ERR|invalid local-forward format"))
		return
	}
	targetAddr := parts[2]

	if !isTargetAllowed(token, targetAddr) {
		log.Printf("Auth failed: target '%s' not allowed for token from %s", targetAddr, remoteAddr)
		conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("ERR|target '%s' not allowed", targetAddr)))
		return
	}

	log.Printf("Local forward: %s → %s (from %s)", remoteAddr, targetAddr, remoteAddr)
	conn.WriteMessage(websocket.TextMessage, []byte("OK|connected"))

	tcpConn, err := net.Dial("tcp", targetAddr)
	if err != nil {
		log.Println("TCP connect error:", err)
		conn.WriteMessage(websocket.TextMessage, []byte("ERR|tcp connect failed: "+err.Error()))
		return
	}
	defer tcpConn.Close()

	go tcpToWS(tcpConn, conn)
	wsToTCP(conn, tcpConn)
}

// --- Remote forward handler ---

func handleRemoteForward(conn *websocket.Conn, token *TokenConfig, parts []string, remoteAddr string) {
	// Format: token|remote-forward|expose|target
	if len(parts) != 4 {
		conn.WriteMessage(websocket.TextMessage, []byte("ERR|invalid remote-forward format"))
		return
	}
	exposeAddr := parts[2]
	clientTarget := parts[3]

	if !isTargetAllowed(token, clientTarget) {
		log.Printf("Auth failed: target '%s' not allowed for token from %s", clientTarget, remoteAddr)
		conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("ERR|target '%s' not allowed", clientTarget)))
		return
	}

	exposed.Lock()
	if _, ok := exposed.listeners[exposeAddr]; ok {
		exposed.Unlock()
		conn.WriteMessage(websocket.TextMessage, []byte("ERR|port already exposed"))
		return
	}
	ln, err := net.Listen("tcp", exposeAddr)
	if err != nil {
		exposed.Unlock()
		conn.WriteMessage(websocket.TextMessage, []byte("ERR|expose failed: "+err.Error()))
		return
	}
	exposed.listeners[exposeAddr] = ln
	exposed.Unlock()

	log.Printf("Remote forward: expose %s → client %s (from %s)", exposeAddr, clientTarget, remoteAddr)
	conn.WriteMessage(websocket.TextMessage, []byte("OK|exposed "+exposeAddr))

	defer func() {
		ln.Close()
		exposed.Lock()
		delete(exposed.listeners, exposeAddr)
		exposed.Unlock()
		log.Printf("Remote forward closed: %s", exposeAddr)
	}()

	for {
		c, err := ln.Accept()
		if err != nil {
			log.Println("Accept error:", err)
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			conn.WriteMessage(websocket.TextMessage, []byte("newconn|"+clientTarget))
			go tcpToWS(c, conn)
			wsToTCP(conn, c)
		}(c)
	}
}

// --- Relay helpers ---

func tcpToWS(tcp net.Conn, ws *websocket.Conn) {
	buf := make([]byte, 4096)
	for {
		n, err := tcp.Read(buf)
		if err != nil {
			return
		}
		if err := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
			return
		}
	}
}

func wsToTCP(ws *websocket.Conn, tcp net.Conn) {
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		if _, err := tcp.Write(data); err != nil {
			return
		}
	}
}
