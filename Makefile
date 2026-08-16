# OpenRMM — docker compose + dev workflow
# Mode comes from .env (OPENRMM_MODE=dev|prod); default dev.

-include .env
OPENRMM_MODE ?= dev
COMPOSE = docker compose -f docker-compose.yml -f docker-compose.$(OPENRMM_MODE).yml

.PHONY: help up down dev logs ps build restart migrate revision psql nats-cli \
        seed admin enroll-token api-key test lint fmt openapi agent release

help: ## Show targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

up: ## Start the stack in current mode
	$(COMPOSE) up -d --build
	$(COMPOSE) logs --tail 20

dev: ## Start the dev stack (hot reload)
	@test "$(OPENRMM_MODE)" = "dev" || (echo "OPENRMM_MODE is '$(OPENRMM_MODE)', not dev (edit .env)"; exit 1)
	$(MAKE) up

down: ## Stop the stack
	$(COMPOSE) down

logs: ## Tail logs (SVC=name to filter)
	$(COMPOSE) logs -f $(SVC)

ps: ## Show service status
	$(COMPOSE) ps

build: ## Rebuild images
	$(COMPOSE) build

restart: ## Restart services (SVC=name to filter)
	$(COMPOSE) restart $(SVC)
	$(COMPOSE) logs --tail 20 $(SVC)

migrate: ## Apply database migrations
	$(COMPOSE) exec openrmm-api alembic upgrade head

revision: ## Autogenerate a migration: make revision m="add devices"
	$(COMPOSE) exec openrmm-api alembic revision --autogenerate -m "$(m)"

psql: ## Postgres shell
	$(COMPOSE) exec openrmm-postgres psql -U $(POSTGRES_USER) -d $(POSTGRES_DB)

nats-cli: ## NATS box for debugging (nats CLI inside the network)
	docker run --rm -it --network $(COMPOSE_PROJECT_NAME)_openrmm-internal natsio/nats-box -s nats://openrmm-nats:4222

admin: ## Create an admin user: make admin EMAIL=you@example.com
	$(COMPOSE) exec openrmm-api openrmm create-admin $(EMAIL)

enroll-token: ## Mint an agent enrollment token
	$(COMPOSE) exec openrmm-api openrmm gen-enrollment-token

api-key: ## Mint an API key: make api-key NAME=claude SCOPES=devices:read,alerts:read
	$(COMPOSE) exec openrmm-api openrmm create-api-key $(NAME) --scopes "$(SCOPES)"

test: ## Run server tests + agent tests
	cd server && uv run pytest -q
	cd agent && go test ./...

lint: ## Lint everything
	cd server && uv run ruff check src tests && uv run ruff format --check src tests
	cd agent && go vet ./...

fmt: ## Format everything
	cd server && uv run ruff check --fix src tests && uv run ruff format src tests
	cd agent && gofmt -w .

openapi: ## Dump the OpenAPI schema for web client generation
	cd server && uv run python -c "import json; from openrmm.api.app import app; print(json.dumps(app.openapi()))" > ../web/openapi.json

agent: ## Build the agent for the local platform
	cd agent && go build -trimpath -o bin/openrmm-agent ./cmd/openrmm-agent

release: ## Tag a CalVer release
	git tag $$(date +%Y.%m.%d)
