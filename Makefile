.PHONY: help init build build-backend build-frontend dev dev-backend dev-frontend run test lint clean docker-build docker-up docker-down docker-logs format tidy install

APP_NAME := yuarcloud
BINARY  := bin/$(APP_NAME)
CONFIG  := configs/config.yaml

help: ## Show this help message
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

init: ## Initialize config from example
	@cp -n configs/config.yaml.example configs/config.yaml && echo "Created configs/config.yaml" || echo "configs/config.yaml already exists"
	@mkdir -p data/files data/tmp/chunks logs bin

install: ## Download Go dependencies
	go mod download
	cd web && npm install

tidy: ## Tidy Go modules
	go mod tidy

# --- Build ---

build: build-backend build-frontend ## Build backend and frontend

build-backend: ## Build Go binary
	@mkdir -p bin
	go build -ldflags="-s -w" -o $(BINARY) ./cmd/server
	@echo "Built $(BINARY)"

build-frontend: ## Build React frontend
	cd web && npm run build
	@echo "Frontend built to web/dist"

# --- Development ---

dev: ## Run both backend and frontend (dev mode)
	@echo "Starting backend on :8080 and frontend on :3000..."

run: build ## Build everything and run
	./$(BINARY) $(CONFIG)

dev-backend: ## Run backend only with hot reload
	go run ./cmd/server $(CONFIG)

dev-frontend: ## Run frontend dev server
	cd web && npm run dev

# --- Quality ---

test: ## Run Go tests
	go test ./... -v

lint: ## Lint Go code
	go vet ./...

format: ## Format Go code
	gofmt -w ./cmd ./internal

clean: ## Remove build artifacts
	rm -rf bin/ web/dist/ web/node_modules/

# --- Docker ---

docker-build: ## Build Docker image
	docker build -t $(APP_NAME):latest .

docker-up: ## Start with docker compose
	docker compose up -d --build

docker-down: ## Stop docker compose
	docker compose down

docker-logs: ## View docker logs
	docker compose logs -f

docker-shell: ## Open shell in container
	docker compose exec yuarcloud sh