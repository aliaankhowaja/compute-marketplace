#!/usr/bin/env node
import { fileURLToPath } from "node:url";
import path from "node:path";

import { main } from "./proxy.js";

const entrypoint = process.argv[1] ? path.resolve(process.argv[1]) : "";
const thisFile = path.resolve(fileURLToPath(import.meta.url));

/* Bin resolves to dist/index.js; tests/l Vitest import proxy.ts directly — skip auto-main there */
const invokedDirectly = entrypoint === thisFile || entrypoint.endsWith(`${path.sep}sockt-mcp-proxy`);

if (invokedDirectly) {
  main().catch((e) => {
    console.error(e);
    process.exit(1);
  });
}
