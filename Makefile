BINARY := bin/gateway
PKG    := ./cmd/gateway

# .PHONY tells make these targets are commands, not files to be produced.
# Without it, a file named "test" in the repo would stop `make test` working.
.PHONY: build run mock run-mock db-up db-down test vet fmt tidy clean all

all: fmt vet test build

build:
	go build -o $(BINARY) $(PKG)

run: build
	./$(BINARY)

# Fake OpenAI/Groq/Anthropic/Gemini on 127.0.0.1:9090. Pair with run-mock.
mock:
	go run ./cmd/mockprovider

# The gateway pointed at the mock provider: no real keys, no spend.
run-mock: build
	MOCK_API_KEY=mock GATEWAY_CONFIG=config.mock.yaml ./$(BINARY)

# Local Postgres from deployments/docker-compose.yml. --wait blocks until the
# healthcheck passes, so `make db-up migrate` works in one go.
db-up:
	docker compose -f deployments/docker-compose.yml up -d --wait

db-down:
	docker compose -f deployments/docker-compose.yml down

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
