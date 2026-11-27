// Usage: node review-verdict.mjs <reviewer-output.md> <diff.patch> <review-out.json>
// Findings never fail the check. Only invalid does, because then no review happened.
import { readFileSync, writeFileSync } from "node:fs";
import { pathToFileURL } from "node:url";

const severities = ["blocking", "should-fix", "nit"];
const headings = { pass: "Looks good.", blocked: "Needs changes.", invalid: "No review." };
const labels = { blocking: "Blocking", "should-fix": "Should fix", nit: "Nit" };
const maxBodyChars = 60_000; // GitHub rejects a review body over 65,536 characters.

export function parseVerdict(text) {
  const blocks = [...text.matchAll(/```json\s*\n([\s\S]*?)```/g)];
  if (blocks.length === 0) {
    return { result: "invalid", error: "the reviewer output has no json verdict block" };
  }
  let v;
  try {
    v = JSON.parse(blocks.at(-1)[1]);
  } catch (err) {
    return { result: "invalid", error: `the verdict is not valid JSON: ${err.message}` };
  }
  if (!["pass", "fail"].includes(v?.verdict) || typeof v.summary !== "string" || !Array.isArray(v.findings)) {
    return { result: "invalid", error: "the verdict needs verdict (pass or fail), summary and findings" };
  }
  for (const f of v.findings) {
    if (!severities.includes(f?.severity) || typeof f.title !== "string") {
      return { result: "invalid", error: `a finding has no valid severity or title: ${JSON.stringify(f)}` };
    }
  }
  const blocked = v.verdict === "fail" || v.findings.some((f) => f.severity === "blocking");
  return { result: blocked ? "blocked" : "pass", summary: v.summary, findings: v.findings };
}

// GitHub rejects a whole review if one comment is on a line outside the diff.
export function commentableLines(diff) {
  const lines = new Map();
  let file = null;
  let oldLeft = 0;
  let newLeft = 0;
  let next = 0;
  for (const row of diff.split("\n")) {
    if (oldLeft > 0 || newLeft > 0) {
      if (row.startsWith("-")) {
        oldLeft--;
      } else if (row.startsWith("+")) {
        newLeft--;
        lines.get(file)?.add(next++);
      } else if (row.startsWith(" ")) {
        oldLeft--;
        newLeft--;
        lines.get(file)?.add(next++);
      }
    } else if (row.startsWith("diff --git ")) {
      file = null;
    } else if (row.startsWith("+++ b/")) {
      file = row.slice("+++ b/".length).replace(/\t$/, "");
      lines.set(file, new Set());
    } else if (row.startsWith("@@ ")) {
      const [, oldCount, start, newCount] = /^@@ -\d+(?:,(\d+))? \+(\d+)(?:,(\d+))? @@/.exec(row);
      oldLeft = Number(oldCount ?? 1);
      newLeft = Number(newCount ?? 1);
      next = Number(start);
    }
  }
  return lines;
}

// GitHub notifies every @name in a comment, and the model can write any name.
const quiet = (s) => String(s ?? "").replace(/@(?=\w)/g, "@\u200b");

const location = (f) => (typeof f.file === "string" ? `\`${quiet(f.file)}${Number.isInteger(f.line) ? `:${f.line}` : ""}\`` : "");

const findingText = (f) =>
  [`**${labels[f.severity]}: ${quiet(f.title).replace(/\.$/, "")}.**`, f.detail && quiet(f.detail), f.suggestion && quiet(f.suggestion)]
    .filter(Boolean)
    .join(" ");

export function buildReview(parsed, reviewerText, diff) {
  if (parsed.result === "invalid") {
    const output = quiet(reviewerText).slice(0, maxBodyChars);
    const body = [`**${headings.invalid}** The reviewer gave no valid verdict, because ${quiet(parsed.error)}.`, "<details><summary>Reviewer output</summary>", output, "</details>"];
    return { event: "COMMENT", body: body.join("\n\n"), comments: [] };
  }

  if (!parsed.findings.some((f) => f.severity !== "nit")) {
    return { event: "COMMENT", body: "LGTM", comments: [] };
  }
  const lines = commentableLines(diff);
  const ordered = severities.flatMap((sev) => parsed.findings.filter((f) => f.severity === sev));
  const inline = ordered.filter((f) => lines.get(f.file)?.has(f.line));
  const outside = ordered.filter((f) => !inline.includes(f));

  const body = [`**${headings[parsed.result]}** ${quiet(parsed.summary)}`];
  if (outside.length > 0) {
    body.push("Findings outside the diff:", ...outside.map((f) => [location(f), findingText(f)].filter(Boolean).join(" ")));
  }
  return {
    event: "COMMENT",
    body: body.join("\n\n").slice(0, maxBodyChars),
    comments: inline.map((f) => ({ path: f.file, line: f.line, side: "RIGHT", body: findingText(f) })),
  };
}

if (import.meta.url === pathToFileURL(process.argv[1]).href) {
  const [input, diffFile, output] = process.argv.slice(2);
  const text = readFileSync(input, "utf8");
  const parsed = parseVerdict(text);
  writeFileSync(output, JSON.stringify(buildReview(parsed, text, readFileSync(diffFile, "utf8"))));
  console.log(parsed.result);
}
