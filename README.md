# tanel

Dynamic TCP WebSocket Tunnel — port forwarding ala SSH -L / -R, built in Go.

## Fitur
- Local forward (SSH -L): listen port lokal, relay ke target TCP via server
- Remote forward (SSH -R): server expose port, relay ke endpoint lokal di client
- Autentikasi token: setiap koneksi wajib kirim token
- Multi-token: server bisa daftarkan banyak token
- Permission per token: batasi mode (local-forward / remote-forward / keduanya) dan whitelist target IP:port
- Config via YAML atau CLI params

## Struktur
- `server.go` — WebSocket server, validasi token, relay TCP
- `client.go` — Multi-tunnel launcher, baca YAML atau CLI params
- `server.yaml` — Contoh config server (token, mode, target restrictions)
- `tanel.yaml` — Contoh config client (remote, token, tunnels)

## Server

### Via YAML
```bash
go run server.go --config server.yaml
```

### Via CLI
```bash
go run server.go --listen :9000 --token mysecrettoken
```
Token via CLI mendapat akses penuh (semua mode, semua target).

### Format server.yaml
```yaml
listen: ":9000"
tokens:
  - value: "abcd1234"
    modes: ["local-forward"]
    targets:
      - "127.0.0.1:3306"
      - "192.168.0.110:7000"

  - value: "efgh5678"
    modes: ["remote-forward"]
    targets:
      - "10.10.1.1:3030"

  - value: "fullaccess"
    modes: ["local-forward", "remote-forward"]
    targets: ["*"]   # semua target dibolehkan
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
tunnels:
  - mode: local-forward
    local: ":8080"
    target: "127.0.0.1:3306"

  - mode: remote-forward
    expose: ":10000"
    target: "10.10.1.1:3030"

  - mode: local-forward
    local: ":9090"
    target: "192.168.0.110:7000"
```

## Kebutuhan
- Go ≥ 1.19
- github.com/gorilla/websocket
- gopkg.in/yaml.v3

---
Generated oleh Bian Solt
