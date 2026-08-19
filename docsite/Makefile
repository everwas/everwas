# Everwas docs site — cookie-cutter targets.

SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z0-9_-]+:.*##/ {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: prod
prod: ## Build + run the production docs container (Caddy serves dist/)
	docker compose up -d --build everwas-docs

.PHONY: dev
dev: ## Run the Astro dev server with HMR (--profile dev)
	docker compose --profile dev up --build everwas-docs-dev

.PHONY: down
down: ## Stop and remove the docs containers
	docker compose --profile dev down
	docker compose down

.PHONY: logs
logs: ## Tail logs (works for whichever profile is up)
	docker compose logs -f --tail=100

.PHONY: build
build: ## Build the static site WITHOUT bringing up Caddy (CI gate)
	docker compose build everwas-docs
