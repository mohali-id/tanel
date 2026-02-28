.PHONY: all build clean server client

BINARY_SERVER = tanel-server
BINARY_CLIENT = tanel

all: build

build: server client

server:
	go build -o $(BINARY_SERVER) server.go

client:
	go build -o $(BINARY_CLIENT) client.go

clean:
	rm -f $(BINARY_SERVER) $(BINARY_CLIENT)
