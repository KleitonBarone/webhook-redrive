import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

export function object(value: unknown): Record<string, unknown> {
  assert(value !== null && typeof value === "object" && !Array.isArray(value), "expected an object");
  return value as Record<string, unknown>;
}

export function text(value: unknown): string {
  assert(typeof value === "string", "expected a string");
  return value;
}

const config = object(JSON.parse(readFileSync(process.env.CONSUMER_CONFIG ?? "/local/consumer.json", "utf8")));
export const secret = text(config.secret);
export const controlToken = text(config.control_token);
export const apiOrigin = "https://consumer:8443";

export function credentials() {
  const value = object(JSON.parse(readFileSync("/local/credentials.json", "utf8")));
  return { producer: text(value.producer), operator: text(value.operator), operatorPrincipal: text(value.operator_principal) };
}
