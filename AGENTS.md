# AGENTS.md

This file tells coding agents how to work in this repository. Human contributors follow the same rules. `CLAUDE.md` imports this file. Reviews use `REVIEW.md`.

The service receives an audio recording over HTTP and stores it as a job in the database (Postgres). A worker sends the audio to an AI provider and gets a transcript and a visit report. The worker then sends the report to a destination.

## Project map

| Path | Contents |
|---|---|
| `cmd/server` | `main` and `run`. The composition root: the only place that reads config and connects the parts. |
| `internal/config` | Reads environment variables one time and validates them. |
| `internal/job` | The `Job` type, the status machine, the Postgres queue and the embedded SQL schema. |
| `internal/httpapi` | Gin routes and handlers: upload, status and health. |
| `internal/worker` | The worker pool and the pipeline. Declares the `Store` and `Sender` interfaces that it uses. |
| `internal/ai` | The `Provider` interface, `Result`, `VisitReport`, the prompt and the JSON schema. |
| `internal/ai/openai`, `internal/ai/gemini` | One AI provider each. |
| `internal/deliver/webhook`, `internal/deliver/hubspot` | One destination each. |
| `internal/httpjson` | Sends a request to a JSON API and classifies HTTP errors. |
| `testdata/visit-note.m4a` | The sample recording. A rep speaks notes after a visit. The people in it are fictional. |

## Commands

| Command | What it does |
|---|---|
| `make check` | Runs `go vet`, `golangci-lint`, the unit tests with `-race` and `govulncheck`. |
| `make test-integration` | Starts the database (Postgres) with Docker Compose. Then runs all tests, including the integration tests, against the separate `debrief_test` database. |
| `make run` | Builds the image and starts the service with its database (Postgres) in Docker Compose. |

A change is done when `make check` passes. If the change touches `internal/job`, `make test-integration` must also pass.

## Architecture rules

- `run` in `cmd/server/main.go` is the only place that selects an AI provider or a destination. It uses one `switch` for each.
- `depguard` in `.golangci.yml` enforces this rule. `internal/job`, `internal/httpapi` and `internal/worker` cannot import a provider or a destination. A provider or a destination cannot import another one.
- The `exhaustive` linter fails the build if a `switch` in `run` does not handle every provider and every destination.
- Declare an interface in the package that uses it. Return concrete types from constructors.
- Change a job status only through the `job.Store` methods in `internal/job/queue.go`. Each method that writes for a claimed job checks the attempt number, so a worker that lost its lease cannot write.
- Do not hold a database transaction or row lock during a call to an AI provider or a destination.
- Every step of the pipeline must be safe to run again. The HubSpot destination is the one known exception, because HubSpot has no idempotency key for HubSpot notes. Save the AI result before delivery, so a retry does not call the model again.
- Do not add packages named `utils`, `common`, `models` or `services`. Do not add a `pkg` directory.

## Go conventions

- Pass `context.Context` as the first parameter. Do not store a context in a struct.
- Set a deadline on every call to an external service.
- Wrap errors with `fmt.Errorf("claim job: %w", err)`. Use lowercase text without "failed to". Use `errors.Is` and `errors.As` to examine errors.
- Handle an error one time. Return it or log it, not both.
- Use sentinel errors named `ErrX` for conditions that callers check.
- Do not use `panic`, `init` functions or package-level state that changes after startup. Read-only package-level values are fine: lookup tables, compiled regexps, sentinel errors and test fixtures. Only `main` calls `os.Exit`.
- Use short package names without repetition: `job.Store`, not `job.JobStore`.
- Log with `log/slog` as JSON. Log job IDs, statuses, durations and error text.

## Code style

- Write code that reads without comments. A name says what a thing is or what it guarantees.
- Give a function one job: check the input, do the work, return. If a function needs a comment to explain its steps, split it.
- Use guard clauses and return early. Keep nesting to one level.
- Put package-level declarations at the top of the file, in one `type`, one `const` and one `var` block.
- Give similar functions the same shape, so that one explains the other.
- Write a comment only for a fact from outside the code, such as how a vendor or the database behaves or what a retry or a stale worker would do, with its consequence, in one line. Never describe this code's own behaviour or design, and never repeat what the code or a name says.

## Testing rules

- Write table-driven tests. Use `httptest` for HTTP servers and clients.
- Write fakes by hand. Do not use mock frameworks or assertion libraries. `go-cmp` is allowed.
- Call the code like its users do. Compare the result with a literal expected value.
- Put Postgres tests behind the `integration` build tag. Run them with `make test-integration`.
- When you add a behavior, add a test that fails without it.

## Security and privacy

- Recordings contain customer data. Do not log audio, transcripts or reports.
- Do not put a vendor error body in an error message or a log line. It can contain request data or part of a key. `httpjson.StatusError` keeps the body in `ResponseBody()`, and only the job store writes it, to the `last_error_body` column.
- Read secrets only from environment variables. Do not commit `.env`.
- Validate all external input at the boundary: HTTP requests, config and model output.
- Parse model output with `ai.ParseReport`. Treat an empty or invalid model answer as an error, never as a success.

## Add an AI provider

1. Create the package `internal/ai/<name>`.
2. Make a `Client` type with an `Analyze` method that satisfies `ai.Provider`.
3. Take each model name as an argument of `New`. Do not hard-code it.
4. Parse the model output with `ai.ParseReport`.
5. Add tests that use an `httptest` server as the vendor API.
6. In `internal/config`, add the name, and a model setting and an allowlist for each model that the provider calls. Test each model with `testdata/visit-note.m4a` before you add it to the allowlist. Add one `case` to the provider `switch` in `run`.
7. Add the package to the `depguard` lists in `.golangci.yml`.

## Add a destination

1. Create the package `internal/deliver/<name>`.
2. Make a `Sender` type with a `Send` method that satisfies `worker.Sender`.
3. If the destination supports an idempotency key, send the job ID as the key.
4. Add tests that use an `httptest` server as the destination.
5. Add the name to `internal/config` and add one `case` to the destination `switch` in `run`.

## Review

The AI review in `.github/workflows/ai-review.yml` runs on each non-draft pull request, and again after each push to it. The rubric is `REVIEW.md`. The workflow runs from the default branch and reads the rubric from there. It never runs code from the pull request.
