BINARY := bin/gateway
PKG    := ./cmd/gateway

# .PHONY tells make these targets are commands, not files to be produced.
# Without it, a file named "test" in the repo would stop `make test` working.
.PHONY: build run mock run-mock test vet fmt tidy clean all

all: fmt vet test build

build:
	go build -o $(BINARY) $(PKG)

run: build
	./$(BINARY)

# Fake OpenAI/Anthropic/Gemini on 127.0.0.1:9090. Pair with run-mock.
mock:
	go run ./cmd/mockprovider

# The gateway pointed at the mock provider: no real keys, no spend.
run-mock: build
	MOCK_API_KEY=mock GATEWAY_CONFIG=config.mock.yaml ./$(BINARY)

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
