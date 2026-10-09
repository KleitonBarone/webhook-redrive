import { createHmac, timingSafeEqual } from "node:crypto";

// Verify the original body before parsing it. Delivery metadata is unsigned.
export function verify(
  secret: string,
  timestamp: string,
  signature: string,
  body: Buffer,
  nowMs: number,
  toleranceMs: number,
): boolean {
  if (!Number.isFinite(nowMs) || !Number.isFinite(toleranceMs) || toleranceMs < 0) return false;
  if (!/^(0|[1-9][0-9]*|-[1-9][0-9]*)$/.test(timestamp)) return false;
  const seconds = Number(timestamp);
  if (!Number.isSafeInteger(seconds) || Math.abs(nowMs - seconds * 1000) > toleranceMs) return false;
  if (!/^v1=[a-fA-F0-9]{64}$/.test(signature)) return false;
  const expected = createHmac("sha256", secret).update(timestamp).update(".").update(body).digest();
  return timingSafeEqual(expected, Buffer.from(signature.slice(3), "hex"));
}
