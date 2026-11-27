GOLANGCI_LINT_VERSION := v2.15.0
GOVULNCHECK_VERSION   := v1.10.0
TEST_DATABASE_URL     ?= postgres://debrief:debrief@localhost:5433/debrief_test?sslmode=disable

.PHONY: run test test-integration lint vuln check

run:
	docker compose up --build

test:
	go test -race ./...

# The tests use their own database, so they never touch the jobs of make run.
test-integration:
	docker compose up -d --wait postgres
	docker compose exec -T postgres createdb -U debrief debrief_test 2>/dev/null || true
	DATABASE_URL="$(TEST_DATABASE_URL)" go test -race -tags integration ./...

lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

check:
	go vet -tags integration ./...
	$(MAKE) lint test vuln
