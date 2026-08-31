.PHONY: help run build test test-race lint fmt up down logs psql redis-cli bench

help: ## Показать список команд
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

build: ## Собрать бинарник
	go build -o bin/server ./cmd/server

run: ## Запустить локально (нужны поднятые postgres и redis)
	go run ./cmd/server

test: ## Прогнать тесты
	go test ./... -count=1

test-race: ## Тесты с детектором гонок
	go test -race ./... -count=1

bench: ## Бенчмарки
	go test -bench=. -benchmem ./...

fmt: ## Форматирование
	gofmt -w .
	go vet ./...

up: ## Поднять всё окружение в Docker
	docker compose up --build -d

down: ## Остановить и удалить контейнеры
	docker compose down -v

logs: ## Логи приложения
	docker compose logs -f app

psql: ## Консоль PostgreSQL
	docker compose exec postgres psql -U shortener -d shortener

redis-cli: ## Консоль Redis
	docker compose exec redis redis-cli
