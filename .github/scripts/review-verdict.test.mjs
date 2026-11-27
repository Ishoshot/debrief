import { test } from "node:test";
import assert from "node:assert/strict";
import { buildReview, commentableLines, parseVerdict } from "./review-verdict.mjs";

const block = (v) => "Some findings in prose.\n\n```json\n" + JSON.stringify(v) + "\n```\n";
const finding = (severity) => ({ severity, file: "internal/job/queue.go", line: 7, title: `${severity} issue` });

test("clean pass", () => {
  const got = parseVerdict(block({ verdict: "pass", summary: "Fine.", findings: [finding("nit")] }));
  assert.equal(got.result, "pass");
});

test("a blocking finding fails even when the verdict says pass", () => {
  const got = parseVerdict(block({ verdict: "pass", summary: "Fine.", findings: [finding("blocking")] }));
  assert.equal(got.result, "blocked");
});

test("a fail verdict blocks", () => {
  assert.equal(parseVerdict(block({ verdict: "fail", summary: "No.", findings: [] })).result, "blocked");
});

test("the last json block wins, so a quoted example earlier in the text cannot pass the gate", () => {
  const text = block({ verdict: "pass", summary: "example", findings: [] }) + block({ verdict: "fail", summary: "real", findings: [] });
  assert.equal(parseVerdict(text).result, "blocked");
});

test("fails closed on missing, broken or incomplete verdicts", () => {
  const cases = {
    "": "the reviewer output has no json verdict block",
    "```json\n{verdict: pass}\n```": "the verdict is not valid JSON: Expected property name or '}' in JSON at position 1 (line 1 column 2)",
    [block({ verdict: "maybe", summary: "", findings: [] })]: "the verdict needs verdict (pass or fail), summary and findings",
    [block({ verdict: "pass", summary: "", findings: [{ severity: "critical", title: "x" }] })]:
      'a finding has no valid severity or title: {"severity":"critical","title":"x"}',
  };
  for (const [text, error] of Object.entries(cases)) {
    assert.deepEqual(parseVerdict(text), { result: "invalid", error });
  }
});

const diff = [
  "diff --git a/internal/job/queue.go b/internal/job/queue.go",
  "--- a/internal/job/queue.go",
  "+++ b/internal/job/queue.go",
  "@@ -5,3 +5,4 @@ import (",
  " \tcontext",
  "-\told",
  "+\tnew",
  "+\tadded",
  " \tend",
  "@@ -40 +41,2 @@ func Claim() {",
  " \tx",
  "+\ty",
  "\\ No newline at end of file",
  "diff --git a/notes.md b/notes.md",
  "new file mode 100644",
  "--- /dev/null",
  "+++ b/notes.md",
  "@@ -0,0 +1,2 @@",
  "+++ b/looks-like-a-header.go",
  "+second line",
  "diff --git a/gone.go b/gone.go",
  "--- a/gone.go",
  "+++ /dev/null",
  "@@ -1 +0,0 @@",
  "-package gone",
  "diff --git a/with space.go b/with space.go",
  "--- a/with space.go\t",
  "+++ b/with space.go\t",
  "@@ -1 +1 @@",
  "-old",
  "+new",
  "diff --git a/logo.png b/logo.png",
  "Binary files a/logo.png and b/logo.png differ",
].join("\n");

test("only the new-side lines of each hunk can take a comment", () => {
  assert.deepEqual(commentableLines(diff), new Map([
    ["internal/job/queue.go", new Set([5, 6, 7, 8, 41, 42])],
    ["notes.md", new Set([1, 2])],
    ["with space.go", new Set([1])],
  ]));
});

test("each finding on a diff line becomes an inline comment, ordered by severity", () => {
  const parsed = parseVerdict(block({
    verdict: "fail",
    summary: "The lease is unsafe.",
    findings: [finding("nit"), { ...finding("blocking"), detail: "Lease is never checked.", suggestion: "Fence on attempts." }],
  }));
  assert.deepEqual(buildReview(parsed, "", diff), {
    event: "COMMENT",
    body: "**Needs changes.** The lease is unsafe.",
    comments: [
      { path: "internal/job/queue.go", line: 7, side: "RIGHT", body: "**Blocking: blocking issue.** Lease is never checked. Fence on attempts." },
      { path: "internal/job/queue.go", line: 7, side: "RIGHT", body: "**Nit: nit issue.**" },
    ],
  });
});

test("a finding outside the diff goes in the summary, because GitHub rejects the whole review otherwise", () => {
  const parsed = parseVerdict(block({
    verdict: "pass",
    summary: "Fine.",
    findings: [
      finding("nit"),
      { ...finding("should-fix"), line: 99, detail: "Not in a hunk." },
      { ...finding("nit"), line: "7` @octocat `" },
      { severity: "nit", title: "No file." },
    ],
  }));
  assert.deepEqual(buildReview(parsed, "", diff), {
    event: "COMMENT",
    body: [
      "**Looks good.** Fine.",
      "Findings outside the diff:",
      "`internal/job/queue.go:99` **Should fix: should-fix issue.** Not in a hunk.",
      "`internal/job/queue.go` **Nit: nit issue.**",
      "**Nit: No file.**",
    ].join("\n\n"),
    comments: [{ path: "internal/job/queue.go", line: 7, side: "RIGHT", body: "**Nit: nit issue.**" }],
  });
});

test("a review with nothing to act on says only LGTM", () => {
  for (const findings of [[], [finding("nit")]]) {
    assert.deepEqual(buildReview(parseVerdict(block({ verdict: "pass", summary: "Fine.", findings })), "", diff), {
      event: "COMMENT",
      body: "LGTM",
      comments: [],
    });
  }
});

test("the review cannot mention people", () => {
  const parsed = parseVerdict(block({
    verdict: "pass",
    summary: "Ping @octocat please.",
    findings: [{ ...finding("should-fix"), title: "Ask @octocat", detail: "cc @hubot" }],
  }));
  const review = buildReview(parsed, "", diff);
  assert.ok(review.body.includes("Ping @\u200boctocat please."));
  assert.equal(review.comments[0].body, "**Should fix: Ask @\u200boctocat.** cc @\u200bhubot");
});

test("an invalid verdict posts no inline comments and cannot mention people", () => {
  const parsed = parseVerdict(block({ verdict: "pass", summary: "", findings: [{ severity: "major", title: "@octocat" }] }));
  const review = buildReview(parsed, "", diff);
  assert.deepEqual(review.comments, []);
  assert.ok(review.body.includes('"title":"@\u200boctocat"'));
});
