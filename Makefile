BINARY := bin/gateway
PKG    := ./cmd/gateway

# The compose database. Override to use another:
#   make migrate DATABASE_URL=postgres://...
DATABASE_URL ?= postgres://gateway:gateway@127.0.0.1:5432/gateway?sslmode=disable

# Admin key for the mock setup only, where nothing real is at stake. `make run`
# takes GATEWAY_ADMIN_KEY from your environment instead.
MOCK_ADMIN_KEY := mock-admin-key-for-local-testing-only

# .PHONY tells make these targets are commands, not files to be produced.
# Without it, a file named "test" in the repo would stop `make test` working.
.PHONY: build run mock run-mock db-up db-down migrate test vet fmt tidy clean all

all: fmt vet test build

build:
	go build -o $(BINARY) $(PKG)

run: build
	DATABASE_URL='$(DATABASE_URL)' ./$(BINARY)

# Fake OpenAI/Groq/Anthropic/Gemini on 127.0.0.1:9090. Pair with run-mock.
mock:
	go run ./cmd/mockprovider

# The gateway pointed at the mock provider: no real keys, no spend. Needs the
# compose database: make db-up migrate.
run-mock: build
	MOCK_API_KEY=mock GATEWAY_ADMIN_KEY=$(MOCK_ADMIN_KEY) DATABASE_URL='$(DATABASE_URL)' \
		GATEWAY_CONFIG=config.mock.yaml ./$(BINARY)

# Local Postgres from deployments/docker-compose.yml. --wait blocks until the
# healthcheck passes, so `make db-up migrate` works in one go.
db-up:
	docker compose -f deployments/docker-compose.yml up -d --wait

db-down:
	docker compose -f deployments/docker-compose.yml down

# Apply pending migrations from internal/db/migrations.
migrate:
	DATABASE_URL='$(DATABASE_URL)' go run ./cmd/migrate

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
