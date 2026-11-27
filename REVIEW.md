# Review rubric

This file is the rubric for every code review in this repository. Human reviewers and the AI review use the same rubric. The AI review reads this file from the default branch, not from the pull request.

## Severity

| Severity | Meaning | What the author does |
|---|---|---|
| `blocking` | The change causes a concrete failure: wrong behavior, data loss, a security hole, a broken build, or a broken rule in `AGENTS.md`. | Fixes it before the merge. |
| `should-fix` | The change works, but it makes the code harder to maintain or test. | Fixes it or replies. |
| `nit` | A small point of taste or wording. | Can ignore it. |

Give each `blocking` finding a failure scenario, or the `AGENTS.md` rule it breaks.

## What to check

### Correctness

- The code does what the attached docs say, for empty, large and duplicate input.
- Job status changes go only through the `job.Store` methods in `internal/job/queue.go`.
- A retry of any step gives the same end state. The HubSpot destination is the one known exception, because HubSpot notes have no idempotency key.
- A repeated upload with the same `Idempotency-Key` returns the same job.

### Architecture

- `cmd/server/main.go` is the only place that selects an AI provider or a destination.
- No new layers, packages or abstractions without a second real caller.

### Simplicity and naming

- The change is as simple as the problem allows. A reviewer can understand each function without reading its callers.
- Names say what a thing is or does.
- If a change makes `README.md` or `docs/DESIGN.md` wrong, the pull request updates that file. Do not ask for a doc change when the attached docs already match the new behavior.

### Concurrency and resources

- Every goroutine has a stop path. Every external call has a deadline.
- No code holds a row lock during a call to an AI provider or a destination.
- Shutdown does not lose a claimed job.

### Tests

- A new behavior has a test that fails without the change.
- A change to SQL in `internal/job` has an integration test.

### Security and privacy

- No log line or error message contains audio, a transcript, an API key or a token. Test fixtures use fictional people and companies, never real customer data, keys or tokens.
- A vendor error body goes only to the `last_error_body` column. It is never logged and never returned by the API.
- The code validates input from HTTP callers and from models at the boundary.

### AI-specific

- Model output goes through `ai.ParseReport` or a stricter parser before use.
- A provider failure never becomes a success. An empty or invalid model answer is an error.
- A change to the prompt, `ai.ReportSchema` or `ai.VisitReport` updates the provider test fixtures in the same pull request.

## Do not flag

- Formatting, import order and other points that `golangci-lint` checks.
- A risk without a concrete failure scenario.
- Personal preference, if the code obeys `AGENTS.md`.
- Code that the pull request does not change, unless the change breaks it.
- A finding that the author already answered with a valid reason in `threads.md`. Raise it again only if the reply misses a concrete failure.

## Output format for the AI reviewer

End the answer with one fenced `json` block that contains only the verdict:

```json
{
  "verdict": "pass",
  "summary": "One or two sentences about the change.",
  "findings": [
    {
      "severity": "should-fix",
      "file": "internal/worker/worker.go",
      "line": 42,
      "title": "Short imperative title, such as Fence the retry on the attempt number",
      "detail": "What goes wrong, as a concrete scenario: the input, the steps and the wrong result.",
      "suggestion": "What to change, and the regression test that would have caught it."
    }
  ]
}
```

- `verdict` is `pass` or `fail`. `fail` means a `blocking` finding, not a failed check.
- `severity` is `blocking`, `should-fix` or `nit`.
- `summary` is one or two sentences to the author about what you found. Do not repeat each finding.
- `file` is the path of the file in the diff. `line` is the line number in the new version of that file. Use a line that the diff shows: an added line or an unchanged line in a hunk. The review posts each finding inline on that line. A finding on another line goes in the review summary.
- `findings` is an empty list if there are no findings.
