BINARY := bin/gateway
PKG    := ./cmd/gateway

# .PHONY tells make these targets are commands, not files to be produced.
# Without it, a file named "test" in the repo would stop `make test` working.
.PHONY: build run test vet fmt tidy clean all

all: fmt vet test build

build:
	go build -o $(BINARY) $(PKG)

run: build
	./$(BINARY)

test:
	go test ./...

vet:
	go vet ./...

fmt:
	go fmt ./...

tidy:
	go mod tidy

clean:
	rm -rf bin
