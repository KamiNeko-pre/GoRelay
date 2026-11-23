import assert from "node:assert/strict";
import test from "node:test";

import * as api from "./api.js";

const jdRules = [
  { id: "nowcoder_jobs", script: "scripts/jobs/nowcoder.lua", url: "https://nowcoder.example" },
  { id: "lever_jobs", script: "scripts/jobs/lever.lua", url: "https://lever.example" },
  { id: "greenhouse_jobs", script: "scripts/jobs/greenhouse.lua", url: "https://greenhouse.example" },
  { id: "ashby_jobs", script: "scripts/jobs/ashby.lua", url: "https://ashby.example" },
  { id: "github_trending_go", script: "scripts/github.lua", url: "https://github.example" },
];

test("createJDBatch submits all four configured JD rules before waiting for a response", async () => {
  assert.equal(typeof api.createJDBatch, "function");

  const originalFetch = globalThis.fetch;
  const calls = [];
  let allRequestsStarted;
  const started = new Promise((resolve) => {
    allRequestsStarted = resolve;
  });
  let releaseResponses;
  const responses = new Promise((resolve) => {
    releaseResponses = resolve;
  });
  globalThis.fetch = async (url, options) => {
    calls.push({ url, body: JSON.parse(options.body) });
    if (calls.length === 4) allRequestsStarted();
    await responses;
    return { ok: true, status: 202, json: async () => ({ data: { id: calls.length } }) };
  };

  try {
    const batch = api.createJDBatch(jdRules, "ui-batch");
    await Promise.race([
      started,
      new Promise((_, reject) => setTimeout(() => reject(new Error("requests were submitted serially")), 100)),
    ]);
    releaseResponses();
    const results = await batch;
    assert.deepEqual(calls.map((call) => call.body.target), [
      "nowcoder_jobs",
      "lever_jobs",
      "greenhouse_jobs",
      "ashby_jobs",
    ]);
    assert.deepEqual(calls.map((call) => call.body.request_key), [
      "ui-batch-nowcoder_jobs",
      "ui-batch-lever_jobs",
      "ui-batch-greenhouse_jobs",
      "ui-batch-ashby_jobs",
    ]);
    assert.equal(results.filter((result) => result.status === "fulfilled").length, 4);
  } finally {
    globalThis.fetch = originalFetch;
  }
});
