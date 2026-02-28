.PHONY: all build clean server client test

BINARY_SERVER = tanel-server
BINARY_CLIENT = tanel

all: build

build: server client

server:
	go build -o $(BINARY_SERVER) server.go

client:
	go build -o $(BINARY_CLIENT) client.go

test:
	go test -v -count=1 ./...

clean:
	rm -f $(BINARY_SERVER) $(BINARY_CLIENT)
