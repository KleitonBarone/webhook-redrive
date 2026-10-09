import assert from "node:assert/strict";
import { createHmac } from "node:crypto";
import { test } from "node:test";
import { verify } from "./signature.ts";

const secret = "synthetic-secret-32-bytes-long";
const timestamp = "1787832000";
const body = Buffer.from('{"amount":42}');
const mac = "v1=ad8d57359db70fdd976ce33f2b7ab6e83e9ce9c1e0857f6db970fe81a3f9ea3b";
const now = Number(timestamp) * 1000;

test("documented wire vector, exact bytes, and constant-size MAC input", () => {
  assert.equal(verify(secret, timestamp, mac, body, now, 300_000), true);
  assert.equal(verify(secret, timestamp, mac.toUpperCase().replace("V1=", "v1="), body, now, 0), true);
  for (const altered of [Buffer.from('{ "amount":42}'), Buffer.from('{"amount":42}\n')]) {
    assert.equal(verify(secret, timestamp, mac, altered, now, 300_000), false);
  }
  assert.equal(verify("another-synthetic-secret", timestamp, mac, body, now, 300_000), false);
  for (const invalid of ["v1=ff", "v2=" + "00".repeat(32), "v1=" + "gg".repeat(32), mac + "," + mac]) {
    assert.equal(verify(secret, timestamp, invalid, body, now, 300_000), false);
  }
});

test("timestamp tolerance includes both boundaries and rejects ambiguous timestamps", () => {
  for (const offset of [-300_000, 300_000]) assert.equal(verify(secret, timestamp, mac, body, now + offset, 300_000), true);
  for (const offset of [-300_001, 300_001]) assert.equal(verify(secret, timestamp, mac, body, now + offset, 300_000), false);
  for (const invalid of ["0" + timestamp, "+" + timestamp, timestamp + " ", "1e9", "9007199254740992"]) {
    const signature = "v1=" + createHmac("sha256", secret).update(invalid + ".").update(body).digest("hex");
    assert.equal(verify(secret, invalid, signature, body, now, 300_000), false);
  }
  assert.equal(verify(secret, timestamp, mac, body, now, -1), false);
  assert.equal(verify(secret, timestamp, mac, body, NaN, 300_000), false);
});
