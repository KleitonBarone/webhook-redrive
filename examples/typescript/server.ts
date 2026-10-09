// Local adoption fixture: HTTPS API gateway and independently verified receiver.
// The SQLite business transaction belongs to this consumer, never the delivery store.
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { request as upstreamRequest } from "node:http";
import { createServer } from "node:https";
import { DatabaseSync } from "node:sqlite";
import { controlToken, object, secret, text } from "./config.ts";
import { verify } from "./signature.ts";

const database = new DatabaseSync("/data/consumer.sqlite");
database.exec(`
  PRAGMA journal_mode=WAL;
  PRAGMA synchronous=FULL;
  CREATE TABLE IF NOT EXISTS receipts (id TEXT PRIMARY KEY, digest TEXT NOT NULL);
  CREATE TABLE IF NOT EXISTS business_actions (id TEXT PRIMARY KEY, executions INTEGER NOT NULL);
  CREATE TABLE IF NOT EXISTS deliveries (id TEXT NOT NULL, digest TEXT NOT NULL, status INTEGER NOT NULL);
`);
let repaired = false;

const server = createServer({ key: readFileSync("/local/server.key"), cert: readFileSync("/local/server.crt") }, async (req, res) => {
  try {
    const path = new URL(req.url ?? "/", "https://consumer:8443").pathname;
    if (path.startsWith("/__test/")) {
      if (req.headers.authorization !== `Bearer ${controlToken}`) { res.writeHead(401).end(); return; }
      if (req.method === "POST" && path === "/__test/repair") repaired = true;
      else if (req.method !== "GET" || path !== "/__test/state") { res.writeHead(404).end(); return; }
      res.setHeader("Content-Type", "application/json");
      res.end(JSON.stringify({
        receipts: database.prepare("SELECT id,digest FROM receipts ORDER BY id").all(),
        actions: database.prepare("SELECT id,executions FROM business_actions ORDER BY id").all(),
        deliveries: database.prepare("SELECT id,digest,status FROM deliveries ORDER BY rowid").all(),
      }));
      return;
    }
    if (path.startsWith("/v1/") || ["/healthz", "/readyz", "/metrics"].includes(path)) {
      const upstream = upstreamRequest(`http://api:8080${req.url}`, { method: req.method, headers: req.headers }, (reply) => {
        res.writeHead(reply.statusCode ?? 502, reply.headers);
        reply.pipe(res);
      });
      upstream.setTimeout(5_000, () => upstream.destroy());
      upstream.on("error", () => { if (!res.headersSent) res.writeHead(503); res.end(); });
      req.on("aborted", () => upstream.destroy());
      req.pipe(upstream);
      return;
    }
    if (req.method !== "POST" || !["/hooks/success", "/hooks/hold", "/hooks/fail"].includes(path)) { res.writeHead(404).end(); return; }
    const chunks: Buffer[] = [];
    let length = 0;
    for await (const chunk of req) {
      const bytes = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk);
      length += bytes.length;
      if (length > 1_048_576) { res.writeHead(413).end(); return; }
      chunks.push(bytes);
    }
    const body = Buffer.concat(chunks);
    const singleHeader = (name: string) => {
      let count = 0;
      for (let i = 0; i < req.rawHeaders.length; i += 2) if (req.rawHeaders[i]?.toLowerCase() === name) count++;
      return count === 1 ? req.headers[name] : undefined;
    };
    const timestamp = singleHeader("x-webhook-timestamp");
    const signature = singleHeader("x-webhook-signature");
    if (typeof timestamp !== "string" || typeof signature !== "string" || !verify(secret, timestamp, signature, body, Date.now(), 300_000)) {
      res.writeHead(401).end(); return;
    }
    const event = object(JSON.parse(body.toString("utf8")));
    const id = text(event.id);
    if (!/^[0-9a-f-]{36}$/.test(id) || event.type !== "adoption.created") { res.writeHead(400).end(); return; }
    const digest = createHash("sha256").update(body).digest("hex");
    if (path === "/hooks/fail" && !repaired) {
      database.prepare("INSERT INTO deliveries VALUES (?,?,503)").run(id, digest);
      res.writeHead(503).end(); return;
    }
    let inserted: boolean;
    database.exec("BEGIN IMMEDIATE");
    try {
      const previous = database.prepare("SELECT digest FROM receipts WHERE id=?").get(id);
      if (previous && previous.digest !== digest) {
        database.exec("ROLLBACK"); res.writeHead(409).end(); return;
      }
      inserted = !previous;
      if (inserted) {
        database.prepare("INSERT INTO receipts VALUES (?,?)").run(id, digest);
        database.prepare("INSERT INTO business_actions VALUES (?,1)").run(id);
      }
      database.prepare("INSERT INTO deliveries VALUES (?,?,204)").run(id, digest);
      database.exec("COMMIT");
    } catch (error) {
      database.exec("ROLLBACK"); throw error;
    }
    // Persist before withholding acknowledgement. The orchestrator kills the worker
    // only after observing the committed receipt, then restarts this consumer too.
    if (path === "/hooks/hold" && inserted) return;
    res.writeHead(204).end();
  } catch {
    if (!res.headersSent) res.writeHead(500);
    res.end();
    console.error("consumer request failed");
  }
});
server.requestTimeout = 10_000;
server.headersTimeout = 5_000;
server.listen(8443, "0.0.0.0", () => console.log("synthetic consumer ready"));
