// Finite public-API client. The orchestrator supplies credentials via private files.
import assert from "node:assert/strict";
import { createHash, randomUUID } from "node:crypto";
import { readFileSync, writeFileSync } from "node:fs";
import { request } from "node:https";
import { setTimeout } from "node:timers/promises";
import { apiOrigin, controlToken, credentials, object, secret, text } from "./config.ts";

const tokens = credentials();
const scenarioFile = "/local/scenarios.json";
async function call(path: string, token: string, expected: number, body?: string, headers: Record<string, string> = {}) {
  const response = await fetch(apiOrigin + path, {
    method: body === undefined ? "GET" : "POST", redirect: "error", signal: AbortSignal.timeout(6_000),
    headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json", ...headers }, body,
  });
  assert.equal(response.status, expected, `unexpected status for ${path}`);
  const raw = await response.text();
  return { value: raw ? object(JSON.parse(raw)) : {}, headers: response.headers };
}
async function state() { return (await call("/__test/state", controlToken, 200)).value; }
async function waitFor(check: () => Promise<boolean>) {
  const deadline = Date.now() + 45_000;
  while (Date.now() < deadline) {
    if (await check()) return;
    await setTimeout(100);
  }
  throw new Error("bounded adoption check did not complete");
}
function scenarios() { return object(JSON.parse(readFileSync(scenarioFile, "utf8"))); }
function rows(value: unknown) { assert(Array.isArray(value)); return value.map(object); }
async function attempts(event: string) {
  return rows((await call(`/v1/events/${event}/attempts`, tokens.operator, 200)).value.attempts);
}
async function completed(event: string, expected: string) {
  await waitFor(async () => (await call(`/v1/events/${event}`, tokens.operator, 200)).value.state === expected);
}
async function submit(endpoint: string, body: string, key: string) {
  return call(`/v1/endpoints/${endpoint}/events`, tokens.producer, 202, body,
    { "X-Event-Type": "adoption.created", "Idempotency-Key": key });
}
async function endpoint(path: string) {
  return (await call("/v1/endpoints", tokens.operator, 201, JSON.stringify({
    url: apiOrigin + path, secret, retry_profile: "demo", max_attempts: 2,
    concurrency_limit: 2, rate_limit: 10, event_ttl_seconds: 300,
  }))).value;
}

try {
  switch (process.argv[2]) {
    case "setup": {
      await call("/v1/endpoints", "wr_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", 401);
      await call("/v1/endpoints", tokens.producer, 403);
      await call("/v1/endpoints", tokens.operator, 400, JSON.stringify({ url: "https://unapproved.invalid/hook", secret }));
      // Explicitly wrong hostname must fail even with the local CA trusted.
      await new Promise<void>((resolve, reject) => {
        const req = request(apiOrigin + "/healthz", { servername: "unapproved.invalid" }, () => reject(new Error("TLS hostname check was bypassed")));
        req.on("error", (error) => {
          if ("code" in error && error.code === "ERR_TLS_CERT_ALTNAME_INVALID") resolve();
          else reject(new Error("unexpected TLS rejection"));
        });
        req.setTimeout(5_000, () => { req.destroy(); reject(new Error("TLS check timed out")); }); req.end();
      });
      const data: Record<string, unknown> = {};
      for (const name of ["success", "hold", "fail", "database"]) {
        const target = await endpoint(`/hooks/${name === "database" ? "success" : name}`);
        const id = randomUUID();
        const body = `{ "id": "${id}", "type": "adoption.created", "private_note": "SYNTHETIC_PAYLOAD_DO_NOT_LOG", "text": "ação", "amount": 42 }\n`;
        data[name] = { endpoint: target.id, version: target.version, id, body, key: randomUUID() };
      }
      const success = object(data.success);
      const first = await submit(text(success.endpoint), text(success.body), text(success.key));
      const repeated = await submit(text(success.endpoint), text(success.body), text(success.key));
      assert.equal(first.headers.get("Idempotency-Replayed"), "false");
      assert.equal(repeated.headers.get("Idempotency-Replayed"), "true");
      assert.equal(text(first.value.id), text(repeated.value.id));
      await call(`/v1/endpoints/${text(success.endpoint)}/events`, tokens.producer, 409, text(success.body).trim(),
        { "X-Event-Type": "adoption.created", "Idempotency-Key": text(success.key) });
      success.event = first.value.id;
      await completed(text(success.event), "succeeded");
      writeFileSync(scenarioFile, JSON.stringify(data), { mode: 0o600 });
      break;
    }
    case "hold": {
      const data = scenarios(), held = object(data.hold);
      held.event = (await submit(text(held.endpoint), text(held.body), text(held.key))).value.id;
      await waitFor(async () => rows((await state()).receipts).some((receipt) => receipt.id === held.id));
      const history = await attempts(text(held.event));
      assert.equal(history.length, 1); assert.equal(history[0]?.state, "in_progress");
      held.attempt = history[0]?.id;
      writeFileSync(scenarioFile, JSON.stringify(data), { mode: 0o600 });
      break;
    }
    case "recovered": {
      const held = object(scenarios().hold);
      await completed(text(held.event), "succeeded");
      const history = await attempts(text(held.event));
      assert.equal(history.length, 1); assert.equal(history[0]?.id, held.attempt);
      assert.equal(history[0]?.claim_count, 2);
      const observed = await state();
      assert.equal(rows(observed.deliveries).filter((row) => row.id === held.id).length, 2);
      assert.equal(rows(observed.actions).find((row) => row.id === held.id)?.executions, 1);
      break;
    }
    case "queued": {
      const data = scenarios(), queued = object(data.database);
      const paused = (await call(`/v1/endpoints/${text(queued.endpoint)}/pause`, tokens.operator, 200,
        JSON.stringify({ expected_version: queued.version, reason: "Synthetic database interruption" }))).value;
      queued.version = paused.version;
      queued.event = (await submit(text(queued.endpoint), text(queued.body), text(queued.key))).value.id;
      writeFileSync(scenarioFile, JSON.stringify(data), { mode: 0o600 });
      break;
    }
    case "unavailable": {
      await call("/healthz", "", 200); await call("/readyz", "", 503);
      const queued = object(scenarios().database);
      await call(`/v1/endpoints/${text(queued.endpoint)}/events`, tokens.producer, 503, text(queued.body),
        { "X-Event-Type": "adoption.created", "Idempotency-Key": text(queued.key) });
      break;
    }
    case "database-recovered": {
      const queued = object(scenarios().database);
      const receipt = await submit(text(queued.endpoint), text(queued.body), text(queued.key));
      assert.equal(receipt.value.id, queued.event); assert.equal(receipt.headers.get("Idempotency-Replayed"), "true");
      await call(`/v1/endpoints/${text(queued.endpoint)}/resume`, tokens.operator, 200,
        JSON.stringify({ expected_version: queued.version, reason: "Synthetic database restored" }));
      await completed(text(queued.event), "succeeded");
      break;
    }
    case "receiver-recovered": {
      const failed = object(scenarios().fail);
      const event = text((await submit(text(failed.endpoint), text(failed.body), text(failed.key))).value.id);
      await completed(event, "dead_letter");
      const before = await attempts(event); assert.equal(before.length, 2);
      await call("/__test/repair", controlToken, 200, "{}");
      const replay = JSON.stringify({ attempt_id: before[1]?.id, request_id: randomUUID(), reason: "Synthetic receiver repaired" });
      await call(`/v1/events/${event}/replays`, tokens.producer, 403, replay);
      const first = await call(`/v1/events/${event}/replays`, tokens.operator, 202, replay);
      const repeated = await call(`/v1/events/${event}/replays`, tokens.operator, 202, replay);
      assert.equal(text(first.value.attempt_id), text(repeated.value.attempt_id));
      await completed(event, "succeeded");
      const after = await attempts(event); assert.equal(after.length, 3);
      assert.deepEqual(after.slice(0, 2), before);
      assert.equal(after[2]?.replay_of, before[1]?.id);
      assert.equal(after[2]?.replay_principal_id, tokens.operatorPrincipal);
      break;
    }
    case "report": {
      const data = scenarios(), observed = await state();
      const deliveries = rows(observed.deliveries), actions = rows(observed.actions);
      assert.equal(deliveries.length, 7); assert.equal(actions.length, 4);
      for (const value of Object.values(data)) {
        const scenario = object(value), digest = createHash("sha256").update(text(scenario.body)).digest("hex");
        assert(deliveries.filter((row) => row.id === scenario.id).every((row) => row.digest === digest));
        assert.equal(actions.find((row) => row.id === scenario.id)?.executions, 1);
      }
      console.log(JSON.stringify({ events: 4, verified_deliveries: 7, business_actions: 4, exact_bytes: true,
        keyed_acceptance_preserved: true, duplicate_processing_prevented: true, worker_claim_recovered: true,
        database_reconnected: true, audited_replay_preserved_history: true, tls_hostname_checked: true }));
      break;
    }
    default: throw new Error("unknown adoption phase");
  }
} catch {
  console.error(`adoption phase failed: ${process.argv[2] ?? "missing"}`);
  process.exitCode = 1;
}
