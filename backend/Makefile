.PHONY: build run test clean docker-build docker-up docker-down migrate push-aliyun

# Variables
APP_NAME=clawreef
BINARY=bin/server
MAIN_FILE=cmd/server/main.go

# Build the application
build:
	go build -o $(BINARY) $(MAIN_FILE)

# Run the application
run:
	go run $(MAIN_FILE)

# Run tests
test:
	go test -v ./...

# Clean build artifacts
clean:
	rm -rf bin/
	go clean

# Download dependencies
deps:
	go mod download
	go mod tidy

# Format code
fmt:
	go fmt ./...

# Lint code
lint:
	golangci-lint run

# Docker commands
docker-build:
	docker build -t $(APP_NAME):latest -f deployments/docker/Dockerfile .

docker-up:
	docker-compose -f deployments/docker/docker-compose.yml up -d

docker-down:
	docker-compose -f deployments/docker/docker-compose.yml down

# Push image to Aliyun
push-aliyun:
	@cd .. && ./push-image.sh latest

# Database migration (requires mysql client)
migrate:
	for file in internal/db/migrations/*.sql; do mysql -h localhost -u clawreef -pclawreef123 clawreef < $$file; done

# Install tools
install-tools:
	go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest

# Development setup
dev-setup: deps
	@echo "Development environment setup complete"
	@echo "Run 'make docker-up' to start MySQL"
	@echo "Run 'make migrate' to initialize database"
	@echo "Run 'make run' to start the server"
