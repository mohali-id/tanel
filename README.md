# tanel

Dynamic TCP WebSocket Tunnel — port forwarding ala SSH -L / -R, built in Go.

## Fitur
- **Local forward** (SSH -L): listen port lokal, relay ke target TCP via server
- **Remote forward** (SSH -R): server expose port, relay ke endpoint lokal di client
- **Token authentication**: setiap koneksi wajib kirim token
- **Multi-token**: server bisa daftarkan banyak token dengan permission berbeda
- **Permission per token**: batasi mode (local/remote/keduanya) dan whitelist target IP:port
- **Connection limit**: batasi jumlah koneksi aktif per token
- **Bandwidth limit**: throttle bytes/sec per token
- **Token expiry**: token bisa punya masa berlaku (expires_at, format RFC3339)
- **Multiplexing**: satu WebSocket connection bisa handle banyak TCP session
- **Auto-reconnect**: client otomatis reconnect dengan exponential backoff
- **Health check**: endpoint `/health` untuk monitoring
- **Dashboard/status**: endpoint `/status` untuk lihat tunnel aktif, koneksi, dan stats
- **Graceful shutdown**: handle SIGINT/SIGTERM, tutup semua koneksi dengan rapi
- **Logging levels**: debug, info, warn, error
- **Config via YAML atau CLI params**

## Struktur
- `server.go` — WebSocket server, validasi token, relay TCP, multiplexing
- `client.go` — Multi-tunnel launcher, auto-reconnect, multiplexing
- `server.yaml` — Contoh config server
- `tanel.yaml` — Contoh config client

## Server

### Via YAML
```bash
go run server.go --config server.yaml
```

### Via CLI
```bash
go run server.go --listen :9000 --token mysecrettoken --log-level debug
```

### Format server.yaml
```yaml
listen: ":9000"
log_level: "info"
tokens:
  - value: "abcd1234"
    modes: ["local-forward"]
    max_connections: 5
    bandwidth_limit: 1048576  # 1MB/s
    targets:
      - "127.0.0.1:3306"
      - "192.168.0.110:7000"

  - value: "efgh5678"
    modes: ["remote-forward"]
    max_connections: 3
    targets:
      - "10.10.1.1:3030"

  - value: "fullaccess"
    modes: ["local-forward", "remote-forward"]
    targets: ["*"]

  - value: "temptoken"
    modes: ["local-forward"]
    targets: ["*"]
    expires_at: "2026-12-31T23:59:59+07:00"
```

## Client

### Via YAML
```bash
go run client.go --config tanel.yaml
```

### Via CLI (single tunnel)
```bash
# Local forward
go run client.go --remote ws://host:9000/ws --token mytoken --mode local-forward --local :8080 --target 127.0.0.1:3306

# Remote forward
go run client.go --remote ws://host:9000/ws --token mytoken --mode remote-forward --expose :10000 --target 10.10.1.1:3030
```

### Format tanel.yaml
```yaml
remote: "ws://host:9000/ws"
token: "fullaccess"
log_level: "info"
reconnect_delay: 5
reconnect_max_wait: 60
tunnels:
  - mode: local-forward
    local: ":8080"
    target: "127.0.0.1:3306"

  - mode: remote-forward
    expose: ":10000"
    target: "10.10.1.1:3030"
```

## Endpoints

| Endpoint | Method | Deskripsi |
|----------|--------|-----------|
| `/ws` | GET | WebSocket tunnel |
| `/health` | GET | Health check (status + uptime) |
| `/status` | GET | Dashboard: tunnel aktif, koneksi per token, stats |

## Kebutuhan
- Go ≥ 1.19
- github.com/gorilla/websocket
- gopkg.in/yaml.v3

---
Generated oleh Bian Solt
