package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
	"gopkg.in/yaml.v3"
)

// --- Config structures ---

type Config struct {
	Remote  string   `yaml:"remote"`
	Token   string   `yaml:"token"`
	Tunnels []Tunnel `yaml:"tunnels"`
}

type Tunnel struct {
	Mode   string `yaml:"mode"`
	Local  string `yaml:"local,omitempty"`
	Expose string `yaml:"expose,omitempty"`
	Target string `yaml:"target"`
}

// --- CLI flags ---

var (
	configPath = flag.String("config", "", "Path to YAML config file")
	remoteFlag = flag.String("remote", "", "Remote WS tunnel URL")
	tokenFlag  = flag.String("token", "", "Auth token")
	modeFlag   = flag.String("mode", "", "Tunnel mode: local-forward or remote-forward")
	localFlag  = flag.String("local", "", "Local listen address (local-forward)")
	exposeFlag = flag.String("expose", "", "Expose address on server (remote-forward)")
	targetFlag = flag.String("target", "", "Target TCP address")
)

func main() {
	flag.Parse()

	var cfg Config

	// Load from YAML if provided
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

	// If CLI params specify a single tunnel, add it
	if *modeFlag != "" && *targetFlag != "" {
		t := Tunnel{
			Mode:   *modeFlag,
			Target: *targetFlag,
			Local:  *localFlag,
			Expose: *exposeFlag,
		}
		cfg.Tunnels = append(cfg.Tunnels, t)
	}

	// Validate
	if cfg.Remote == "" {
		log.Fatal("Remote URL is required. Use --remote or set 'remote' in YAML config.")
	}
	if cfg.Token == "" {
		log.Fatal("Token is required. Use --token or set 'token' in YAML config.")
	}
	if len(cfg.Tunnels) == 0 {
		log.Fatal("No tunnels defined. Use --config or CLI params (--mode, --target, etc).")
	}

	log.Printf("Connecting to %s (%d tunnel(s))", cfg.Remote, len(cfg.Tunnels))

	var wg sync.WaitGroup
	for _, t := range cfg.Tunnels {
		switch t.Mode {
		case "local-forward":
			wg.Add(1)
			go func(t Tunnel) {
				defer wg.Done()
				localForward(cfg.Remote, cfg.Token, t.Local, t.Target)
			}(t)
		case "remote-forward":
			wg.Add(1)
			go func(t Tunnel) {
				defer wg.Done()
				remoteForward(cfg.Remote, cfg.Token, t.Expose, t.Target)
			}(t)
		default:
			log.Printf("Unknown mode: %s", t.Mode)
		}
	}
	wg.Wait()
}

// --- Local forward ---

func localForward(remoteUrl, token, localAddr, targetAddr string) {
	ln, err := net.Listen("tcp", localAddr)
	if err != nil {
		log.Printf("TCP listen error (%s): %v", localAddr, err)
		return
	}
	log.Printf("Local forward: %s → %s (via %s)", localAddr, targetAddr, remoteUrl)
	for {
		clientConn, err := ln.Accept()
		if err != nil {
			log.Println("TCP accept error:", err)
			continue
		}
		go handleLocalConn(remoteUrl, token, clientConn, targetAddr)
	}
}

func handleLocalConn(remoteUrl, token string, clientConn net.Conn, targetAddr string) {
	defer clientConn.Close()
	wsConn, _, err := websocket.DefaultDialer.Dial(remoteUrl, nil)
	if err != nil {
		log.Println("WS dial error:", err)
		return
	}
	defer wsConn.Close()

	// Send: token|local-forward|target
	cmd := fmt.Sprintf("%s|local-forward|%s", token, targetAddr)
	if err := wsConn.WriteMessage(websocket.TextMessage, []byte(cmd)); err != nil {
		log.Println("WS write error:", err)
		return
	}

	// Wait for server response
	_, msg, err := wsConn.ReadMessage()
	if err != nil {
		log.Println("WS read error:", err)
		return
	}
	resp := string(msg)
	if strings.HasPrefix(resp, "ERR|") {
		log.Printf("Server rejected: %s", resp)
		return
	}
	log.Printf("Connected: %s → %s", clientConn.RemoteAddr(), targetAddr)

	go tcpToWS(clientConn, wsConn)
	wsToTCP(wsConn, clientConn)
}

// --- Remote forward ---

func remoteForward(remoteUrl, token, exposePort, targetAddr string) {
	wsConn, _, err := websocket.DefaultDialer.Dial(remoteUrl, nil)
	if err != nil {
		log.Printf("WS dial error: %v", err)
		return
	}
	defer wsConn.Close()

	// Send: token|remote-forward|expose|target
	cmd := fmt.Sprintf("%s|remote-forward|%s|%s", token, exposePort, targetAddr)
	if err := wsConn.WriteMessage(websocket.TextMessage, []byte(cmd)); err != nil {
		log.Println("WS write error:", err)
		return
	}

	log.Printf("Remote forward: expose %s → %s", exposePort, targetAddr)

	for {
		_, msg, err := wsConn.ReadMessage()
		if err != nil {
			log.Println("WS read error:", err)
			return
		}
		str := string(msg)
		if strings.HasPrefix(str, "ERR|") {
			log.Printf("Server rejected: %s", str)
			return
		}
		if strings.HasPrefix(str, "OK|") {
			log.Println(str)
			continue
		}
		if strings.HasPrefix(str, "newconn|") {
			target := strings.TrimPrefix(str, "newconn|")
			go handleRemoteConn(wsConn, target)
		} else {
			log.Println("Server:", str)
		}
	}
}

func handleRemoteConn(wsConn *websocket.Conn, targetAddr string) {
	tcpConn, err := net.Dial("tcp", targetAddr)
	if err != nil {
		log.Printf("TCP connect error (%s): %v", targetAddr, err)
		return
	}
	defer tcpConn.Close()
	go tcpToWS(tcpConn, wsConn)
	wsToTCP(wsConn, tcpConn)
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
