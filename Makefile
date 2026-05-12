.PHONY: build run down logs clean

build:
	docker compose --profile build-only build

run:
	docker compose up

down:
	docker compose down

logs:
	docker compose logs -f

clean:
	docker compose down -v --remove-orphans
