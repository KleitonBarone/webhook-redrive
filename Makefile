.PHONY: format format-check vet test test-integration check dependencies demo

format:
	gofmt -w cmd internal migrations signature examples

format-check:
	@test -z "$$(gofmt -l cmd internal migrations signature examples)" || (gofmt -l cmd internal migrations signature examples && exit 1)

vet:
	go vet ./...

test:
	go test -race ./...

test-integration:
	test -n "$$TEST_DATABASE_URL"
	go test -race -count=1 ./internal/store ./internal/integration ./examples/orders

check: format-check vet test

dependencies:
	docker compose up -d postgres

demo:
	docker compose up --build
