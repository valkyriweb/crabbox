import assert from "node:assert/strict";
import { test } from "node:test";
import { retryDatabaseInitialization } from "../node/database-retry.ts";

test("transient connection failures recover without exposing error text", async () => {
  let calls = 0;
  const delays = [];
  const warnings = [];
  const result = await retryDatabaseInitialization(async () => {
    if (++calls < 3) throw Object.assign(new Error("secret connection text"), { code: "ECONNREFUSED" });
    return "ready";
  }, { sleep: async ms => { delays.push(ms); }, warn: text => warnings.push(text) });
  assert.equal(result, "ready");
  assert.deepEqual(delays, [1000, 2000]);
  assert.equal(warnings.length, 2);
  assert.ok(warnings.every(text => !text.includes("secret")));
});

test("persistent transient errors stop after the bounded attempts", async () => {
  let calls = 0;
  const delays = [];
  const failure = Object.assign(new Error("offline"), { code: "57P03" });
  await assert.rejects(retryDatabaseInitialization(async () => {
    calls++;
    throw failure;
  }, { sleep: async ms => { delays.push(ms); }, warn: () => {} }), error => error === failure);
  assert.equal(calls, 6);
  assert.equal(delays.length, 5);
  assert.ok(delays.every(ms => ms <= 10000));
});

test("configuration and authentication errors fail immediately", async () => {
  for (const code of ["28P01", "42501", "42601", undefined]) {
    let calls = 0;
    const failure = Object.assign(new Error("invalid config"), { code });
    await assert.rejects(retryDatabaseInitialization(async () => {
      calls++;
      throw failure;
    }, { sleep: async () => assert.fail("unexpected retry") }), error => error === failure);
    assert.equal(calls, 1);
  }
});
