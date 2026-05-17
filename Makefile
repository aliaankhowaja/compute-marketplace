.PHONY: build run run-debug down logs clean

build:
	docker compose --profile build-only build

run:
	LOG_LEVEL=debug docker compose up --build --force-recreate

run-debug:
	LOG_LEVEL=debug docker compose up --build --force-recreate

down:
	docker compose down

logs:
	docker compose logs -f --timestamps

clean:
	docker compose down -v --remove-orphans
