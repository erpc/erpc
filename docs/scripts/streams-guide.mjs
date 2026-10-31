#!/usr/bin/env node
/**
 * streams-guide.mjs — the streams integration guide's copies.
 *
 *   node docs/scripts/streams-guide.mjs write <file>   writes the guide's markdown to <file>
 *   node docs/scripts/streams-guide.mjs check          fails on drift
 *
 * Every fenced block of docs/pages/operation/streams-integration.mdx names a
 * repo file (```go filename="sdk/go/streams/examples/logs/main.go"```) whose
 * content it is, byte for byte; the examples are compiled and run by the
 * real-world toolkit row T7. Each library ships the page's .llms.txt
 * rendering as INTEGRATION.md: `go generate` in sdk/go/streams, `prepack`
 * in typescript/streams.
 */

import { promises as fs } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { renderPage, SITE_BASE_URL } from "./build-llms.mjs";

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const PAGE_REL = "operation/streams-integration.mdx";
const PAGE = path.join(REPO_ROOT, "docs/pages", PAGE_REL);
const COPIES = ["sdk/go/streams/INTEGRATION.md", "typescript/streams/INTEGRATION.md"];

/** The guide as one markdown file outside the docs site: links absolute. */
async function render() {
	const { header, body } = renderPage(await fs.readFile(PAGE, "utf8"), PAGE_REL);
	return `${header}\n${body}`.replace(/\]\(\//g, `](${SITE_BASE_URL}/`);
}

/** The page's fenced blocks: info string, filename, body, line. */
function fences(src) {
	const out = [];
	const lines = src.split("\n");
	for (let i = 0; i < lines.length; i++) {
		const open = lines[i].match(/^(`{3,})(.*)$/);
		if (!open) continue;
		const close = new RegExp(`^${open[1]}\\s*$`);
		let j = i + 1;
		while (j < lines.length && !close.test(lines[j])) j++;
		const file = open[2].match(/filename="([^"]+)"/)?.[1];
		out.push({ line: i + 1, info: open[2].trim(), file, body: lines.slice(i + 1, j).join("\n") });
		i = j;
	}
	return out;
}

async function check() {
	const problems = [];
	const src = await fs.readFile(PAGE, "utf8");
	const blocks = fences(src);
	if (blocks.length === 0) problems.push(`${PAGE_REL}: no code blocks`);
	for (const b of blocks) {
		const where = `docs/pages/${PAGE_REL}:${b.line}`;
		if (!b.file) {
			problems.push(`${where}: code block without filename="<repo path>"`);
			continue;
		}
		const content = await fs.readFile(path.join(REPO_ROOT, b.file), "utf8").catch(() => null);
		if (content === null) problems.push(`${where}: ${b.file} does not exist`);
		else if (content.replace(/\n$/, "") !== b.body)
			problems.push(`${where}: block differs from ${b.file}`);
	}
	const want = await render();
	for (const copy of COPIES) {
		const got = await fs.readFile(path.join(REPO_ROOT, copy), "utf8").catch(() => null);
		if (got !== want)
			problems.push(`${copy}: differs from the guide; run go generate ./... in sdk/go/streams and pnpm prepack in typescript/streams`);
	}
	for (const p of problems) console.error(p);
	if (problems.length > 0) process.exit(1);
	console.log(`streams guide: ${blocks.length} code blocks match their files; ${COPIES.length} copies current`);
}

const [cmd, arg] = process.argv.slice(2);
if (cmd === "write" && arg) await fs.writeFile(arg, await render());
else if (cmd === "check") await check();
else {
	console.error("usage: streams-guide.mjs write <file> | check");
	process.exit(2);
}
