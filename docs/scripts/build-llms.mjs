#!/usr/bin/env node
/**
 * build-llms.mjs — generate per-page .llms.txt files from MDX sources.
 *
 * For every `docs/pages/**\/*.mdx` the script emits a sibling
 * `docs/public/<same path>.llms.txt` containing the page rendered as pure
 * markdown:
 *   • All <AISection> content fully expanded.
 *   • <ConfigCode> / <ConfigTabs> rendered as fenced markdown code blocks
 *     prefixed with their config path (so an LLM consumer knows WHERE the
 *     snippet goes in the full tree).
 *   • <Tabs> / <Callout> / <Steps> flattened to plain markdown.
 *   • Internal links (/foo) rewritten to /foo.llms.txt so an AI agent can
 *     follow them and stay in the machine-readable surface.
 *
 * Also writes a root `docs/public/llms.txt` listing every page with its
 * URL — overwriting the legacy hand-scraped 244 KB file.
 *
 * JSX parsing uses a small character-based scanner (see `scanTag`) rather
 * than regex. The scanner tracks quote / template-literal / brace nesting,
 * so attribute values may freely contain `>`, `<`, backticks, nested `{...}`
 * expressions, etc.
 */

import { promises as fs } from "node:fs";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { createRequire } from "node:module";

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);
const DOCS_ROOT = path.resolve(__dirname, "..");
const PAGES_DIR = path.join(DOCS_ROOT, "pages");
const PUBLIC_DIR = path.join(DOCS_ROOT, "public");

const SITE_BASE_URL = "https://docs.erpc.cloud";

const require = createRequire(import.meta.url);

/* -------------------------------------------------------------------------- */
/* Walk the pages tree                                                         */
/* -------------------------------------------------------------------------- */

async function walkMdx(dir, rel = "") {
	const entries = await fs.readdir(dir, { withFileTypes: true });
	const results = [];
	for (const entry of entries) {
		const abs = path.join(dir, entry.name);
		const next = rel ? `${rel}/${entry.name}` : entry.name;
		if (entry.isDirectory()) {
			results.push(...(await walkMdx(abs, next)));
		} else if (
			entry.isFile() &&
			(entry.name.endsWith(".mdx") || entry.name.endsWith(".md")) &&
			!entry.name.startsWith("_") &&
			entry.name !== "_meta.js" &&
			!entry.name.startsWith(".")
		) {
			results.push({ abs, rel: next });
		}
	}
	return results;
}

/* -------------------------------------------------------------------------- */
/* Frontmatter                                                                  */
/* -------------------------------------------------------------------------- */

function splitFrontmatter(raw) {
	if (!raw.startsWith("---\n")) return { frontmatter: {}, body: raw };
	const end = raw.indexOf("\n---", 4);
	if (end < 0) return { frontmatter: {}, body: raw };
	const fmText = raw.slice(4, end);
	const body = raw.slice(end + 4).replace(/^\n+/, "");
	const frontmatter = {};
	for (const line of fmText.split("\n")) {
		const m = line.match(/^([A-Za-z0-9_-]+):\s*(.*)$/);
		if (m) frontmatter[m[1]] = m[2].replace(/^["']|["']$/g, "");
	}
	return { frontmatter, body };
}

/* -------------------------------------------------------------------------- */
/* Character-based JSX scanner                                                  */
/* -------------------------------------------------------------------------- */

/** Skip a quoted string starting at `i` (src[i] must be the opening quote). */
function skipString(src, i, quote) {
	i++;
	while (i < src.length) {
		const c = src[i];
		if (c === "\\") {
			i += 2;
			continue;
		}
		if (c === quote) return i + 1;
		i++;
	}
	return i;
}

/** Skip a template literal starting at the backtick. Handles ${...} nesting. */
function skipTemplate(src, i) {
	i++;
	while (i < src.length) {
		const c = src[i];
		if (c === "\\") {
			i += 2;
			continue;
		}
		if (c === "`") return i + 1;
		if (c === "$" && src[i + 1] === "{") {
			i = skipBraces(src, i + 1);
			continue;
		}
		i++;
	}
	return i;
}

/** Skip a {...} block. `src[i]` must be `{`. Handles nested braces, strings, templates. */
function skipBraces(src, i) {
	let depth = 0;
	while (i < src.length) {
		const c = src[i];
		if (c === '"' || c === "'") {
			i = skipString(src, i, c);
			continue;
		}
		if (c === "`") {
			i = skipTemplate(src, i);
			continue;
		}
		if (c === "{") {
			depth++;
			i++;
			continue;
		}
		if (c === "}") {
			depth--;
			i++;
			if (depth === 0) return i;
			continue;
		}
		i++;
	}
	return i;
}

/**
 * Scan the opening tag starting at `src[i]` (must be `<`). Returns
 * { tag, attrsText, selfClose, openEnd } where `openEnd` is the index just
 * past the `>` of the opening tag. Returns null if not a recognizable tag.
 */
function scanOpenTag(src, i) {
	if (src[i] !== "<") return null;
	let j = i + 1;
	// Capture the tag name: starts with a letter, then [A-Za-z0-9_.] (allow Tabs.Tab)
	if (!/[A-Za-z]/.test(src[j])) return null;
	const nameStart = j;
	while (j < src.length && /[A-Za-z0-9_.]/.test(src[j])) j++;
	const tag = src.slice(nameStart, j);
	const attrsStart = j;
	// Now skip through attributes until we hit > or /> at top level
	while (j < src.length) {
		const c = src[j];
		if (c === '"' || c === "'") {
			j = skipString(src, j, c);
			continue;
		}
		if (c === "{") {
			j = skipBraces(src, j);
			continue;
		}
		if (c === "/" && src[j + 1] === ">") {
			const attrsText = src.slice(attrsStart, j).trim();
			return { tag, attrsText, selfClose: true, openEnd: j + 2 };
		}
		if (c === ">") {
			const attrsText = src.slice(attrsStart, j).trim();
			return { tag, attrsText, selfClose: false, openEnd: j + 1 };
		}
		j++;
	}
	return null;
}

/**
 * Find the index after the matching `</tag>` for an opening tag at
 * `openTag.openEnd`. Returns -1 if not found.
 */
function findClose(src, tag, fromIdx) {
	// NOTE: we deliberately do NOT skip quotes/backticks at the top level here.
	// JSX children are markdown — inline code and ``` fences use backticks in
	// ways that do not pair like JS template literals (a fence is 3 backticks),
	// which previously desynchronized the scan. Template literals only occur
	// inside tag ATTRIBUTES, and `scanOpenTag` already skips those safely; so
	// whenever we encounter any opening tag we jump past its attributes via
	// scanOpenTag and continue from there.
	let i = fromIdx;
	let depth = 1;
	while (i < src.length) {
		if (src[i] !== "<") {
			i++;
			continue;
		}
		// Detect </tag>
		if (
			src.startsWith(`</${tag}`, i) &&
			!/[A-Za-z0-9_.]/.test(src[i + 2 + tag.length] ?? "")
		) {
			// skip to >
			let k = i + 2 + tag.length;
			while (k < src.length && src[k] !== ">") k++;
			depth--;
			if (depth === 0) return k + 1;
			i = k + 1;
			continue;
		}
		// Any other opening tag: jump past its attributes (handles nested
		// same-tag depth AND attribute template literals containing markdown).
		const open = scanOpenTag(src, i);
		if (open) {
			if (open.tag === tag && !open.selfClose) depth++;
			i = open.openEnd;
			continue;
		}
		i++;
	}
	return -1;
}

/** Parse the attribute slice of a single opening tag. */
function parseAttrs(attrsText) {
	const out = {};
	let i = 0;
	while (i < attrsText.length) {
		// skip whitespace
		while (i < attrsText.length && /\s/.test(attrsText[i])) i++;
		if (i >= attrsText.length) break;
		// read name
		const nameStart = i;
		while (i < attrsText.length && /[A-Za-z0-9_-]/.test(attrsText[i])) i++;
		const name = attrsText.slice(nameStart, i);
		if (!name) break;
		// skip whitespace
		while (i < attrsText.length && /\s/.test(attrsText[i])) i++;
		if (attrsText[i] !== "=") {
			// boolean attribute
			out[name] = true;
			continue;
		}
		i++; // consume =
		while (i < attrsText.length && /\s/.test(attrsText[i])) i++;
		const valStart = i;
		if (attrsText[i] === '"' || attrsText[i] === "'") {
			const q = attrsText[i];
			i = skipString(attrsText, i, q);
			out[name] = attrsText.slice(valStart + 1, i - 1);
			continue;
		}
		if (attrsText[i] === "{") {
			const end = skipBraces(attrsText, i);
			let inner = attrsText.slice(i + 1, end - 1);
			// Detect template literal: starts/ends with `
			if (inner.startsWith("`") && inner.endsWith("`")) {
				inner = inner.slice(1, -1);
			}
			out[name] = inner;
			i = end;
			continue;
		}
		// unquoted value (rare in JSX)
		while (i < attrsText.length && !/\s/.test(attrsText[i])) i++;
		out[name] = attrsText.slice(valStart, i);
	}
	return out;
}

/**
 * Walk `src` and replace every `<Tag .../>` or `<Tag>...</Tag>` using `handler`.
 * Handler receives ({ attrs, inner }) → string. The function processes the
 * source left-to-right so that handlers operating on outer tags see the
 * already-transformed inner content of inner tags (we transform inner BEFORE
 * outer? No — we just don't recurse here, and rely on transform ordering).
 */
function transformTag(src, tagName, handler) {
	let out = "";
	let i = 0;
	while (i < src.length) {
		if (src[i] !== "<") {
			out += src[i++];
			continue;
		}
		const open = scanOpenTag(src, i);
		if (!open || open.tag !== tagName) {
			out += src[i++];
			continue;
		}
		const attrs = parseAttrs(open.attrsText);
		if (open.selfClose) {
			out += handler({ attrs, inner: "" });
			i = open.openEnd;
			continue;
		}
		const closeEnd = findClose(src, tagName, open.openEnd);
		if (closeEnd < 0) {
			out += src[i++];
			continue;
		}
		// Inner = between openEnd and the start of </tag>
		// Walk back from closeEnd to find the `<` of `</tag>`
		let k = closeEnd - 1;
		while (k >= 0 && src[k] !== "<") k--;
		const inner = src.slice(open.openEnd, k);
		out += handler({ attrs, inner });
		i = closeEnd;
	}
	return out;
}

/* -------------------------------------------------------------------------- */
/* Transforms                                                                   */
/* -------------------------------------------------------------------------- */

function stripImports(src) {
	// Match `import ... from "..."` or `import ... from '...'` even across
	// multiple lines (newlines inside the destructuring braces are common).
	// We anchor on `^import` at the start of a line and walk forward to the
	// terminating quote of the module-specifier string + optional `;`.
	return src.replace(
		/^import\b[\s\S]*?\bfrom\s*(['"])[^'"]*\1\s*;?[ \t]*\n?/gm,
		"",
	);
}

function transformCallout(src) {
	return transformTag(src, "Callout", ({ attrs, inner }) => {
		const type = String(attrs.type ?? "note").toUpperCase();
		const body = inner.trim();
		const prefixed = body
			.split("\n")
			.map((l) => `> ${l}`)
			.join("\n");
		return `\n> **${type}**\n${prefixed}\n`;
	});
}

function transformConfigCode(src) {
	return transformTag(src, "ConfigCode", ({ attrs, inner }) => {
		const lang = attrs.language ?? "";
		// Support both styles:
		//   <ConfigCode code={`...`} ... />
		//   <ConfigCode ...>{`...`}</ConfigCode>
		let codeStr = attrs.code;
		if (!codeStr) {
			const tpl = inner.match(/\{`([\s\S]*?)`\}/);
			codeStr = tpl ? tpl[1] : inner;
		}
		const code = codeStr.replace(/^\n+|\n+$/g, "");
		const header = [];
		if (attrs.path) header.push(`**Config path:** \`${attrs.path}\``);
		if (attrs.filename) header.push(`**File:** \`${attrs.filename}\``);
		const headerStr = header.length > 0 ? header.join(" · ") + "\n\n" : "";
		return `\n${headerStr}\`\`\`${lang}\n${code}\n\`\`\`\n`;
	});
}

function transformConfigTabs(src) {
	return transformTag(src, "ConfigTabs", ({ attrs }) => {
		const yaml = (attrs.yaml ?? "").replace(/^\n+|\n+$/g, "");
		const ts = (attrs.ts ?? "").replace(/^\n+|\n+$/g, "");
		const filenameYaml = attrs.filenameYaml ?? "erpc.yaml";
		const filenameTs = attrs.filenameTs ?? "erpc.ts";
		const out = [];
		if (attrs.path) out.push(`**Config path:** \`${attrs.path}\`\n`);
		if (yaml) {
			out.push(`**YAML — \`${filenameYaml}\`:**\n\n\`\`\`yaml\n${yaml}\n\`\`\`\n`);
		}
		if (ts) {
			out.push(
				`**TypeScript — \`${filenameTs}\`:**\n\n\`\`\`typescript\n${ts}\n\`\`\`\n`,
			);
		}
		return "\n" + out.join("\n") + "\n";
	});
}

function transformAISection(src) {
	return transformTag(src, "AISection", ({ attrs, inner }) => {
		const title = attrs.title ?? "Copy for your AI assistant";
		return `\n\n---\n\n### ${title}\n\n${inner.trim()}\n\n---\n`;
	});
}

function transformTabs(src) {
	return transformTag(src, "Tabs", ({ attrs, inner }) => {
		// items={["yaml", "typescript"]} → parse the brace expression
		const itemsExpr = attrs.items ?? "";
		const labels = [];
		const itemsRe = /["']([^"']+)["']/g;
		let m;
		while ((m = itemsRe.exec(itemsExpr)) !== null) labels.push(m[1]);
		// Each <Tabs.Tab>...</Tabs.Tab> is a tab body in order.
		const tabContents = [];
		let i = 0;
		while (i < inner.length) {
			if (inner[i] !== "<") {
				i++;
				continue;
			}
			const open = scanOpenTag(inner, i);
			if (!open || open.tag !== "Tabs.Tab") {
				i++;
				continue;
			}
			const closeEnd = findClose(inner, "Tabs.Tab", open.openEnd);
			if (closeEnd < 0) {
				i = open.openEnd;
				continue;
			}
			let k = closeEnd - 1;
			while (k >= 0 && inner[k] !== "<") k--;
			tabContents.push(inner.slice(open.openEnd, k));
			i = closeEnd;
		}
		return (
			"\n" +
			tabContents
				.map((body, idx) => {
					const label = labels[idx] ?? `Tab ${idx + 1}`;
					return `**${label}:**\n\n${body.trim()}\n`;
				})
				.join("\n") +
			"\n"
		);
	});
}

function transformSteps(src) {
	return transformTag(src, "Steps", ({ inner }) => `\n${inner.trim()}\n`);
}

/**
 * Expand plain MDX <details>/<summary> blocks (the L2 human deep-dive
 * convention). The summary becomes a bold lead-in line; the body is emitted
 * fully expanded so the .llms.txt surface never hides content.
 */
function transformDetails(src) {
	return transformTag(src, "details", ({ inner }) => {
		let heading = "";
		const withoutSummary = transformTag(inner, "summary", ({ inner: s }) => {
			heading = s.trim();
			return "";
		});
		const body = withoutSummary.trim();
		return heading ? `\n${heading}\n\n${body}\n` : `\n${body}\n`;
	});
}

/**
 * <CapabilityGrid items={[...]}/> → markdown bullet list. The items attr is a
 * JS array literal of {title, href, summary}; parse the string/href/summary
 * triplets leniently rather than evaluating it.
 */
function transformCapabilityGrid(src) {
	return transformTag(src, "CapabilityGrid", ({ attrs }) => {
		const itemsExpr = attrs.items ?? "";
		const itemRe =
			/title:\s*["']([^"']+)["'][\s\S]*?href:\s*["']([^"']+)["'][\s\S]*?summary:\s*["']([^"']+)["']/g;
		const lines = [];
		let m;
		while ((m = itemRe.exec(itemsExpr)) !== null) {
			lines.push(`- **[${m[1]}](${m[2]})** — ${m[3]}`);
		}
		if (lines.length === 0) {
			return "\n> **Capabilities:** see the sub-pages of this section for full detail.\n";
		}
		// Internal hrefs get rewritten to .llms.txt later by rewriteInternalLinks.
		return `\n${lines.join("\n")}\n`;
	});
}

/**
 * <PromptExample n=… title=… prompt={`…`}/> → heading + fenced text block so
 * agents (and the llms surface) keep the copyable prompt verbatim.
 */
function transformPromptExample(src) {
	return transformTag(src, "PromptExample", ({ attrs }) => {
		const n = attrs.n ?? "?";
		const title = attrs.title ?? "";
		const prompt = String(attrs.prompt ?? "").trim();
		return `\n**Prompt Example #${n}: ${title}**\n\n\`\`\`text\n${prompt}\n\`\`\`\n`;
	});
}

/** <SourceLink file=… lines=… label=…/> → plain markdown GitHub permalink. */
function transformSourceLink(src) {
	return transformTag(src, "SourceLink", ({ attrs }) => {
		const file = attrs.file ?? "";
		const lines = attrs.lines ?? "";
		const anchor = lines ? `#L${String(lines).replace(/-/g, "-L")}` : "";
		const label = attrs.label ?? (lines ? `${file}:L${lines}` : file);
		return `[\`${label}\`](https://github.com/erpc/erpc/blob/main/${file}${anchor})`;
	});
}

function stripBareLeftoverComponents(src) {
	// Run the JSX scanner left-to-right; for any unknown capitalized tag,
	// emit just its inner content (drop the wrapper). Doing this with the
	// scanner avoids the regex hazard of attribute values containing `>`.
	let out = "";
	let i = 0;
	while (i < src.length) {
		if (src[i] !== "<") {
			out += src[i++];
			continue;
		}
		const open = scanOpenTag(src, i);
		if (!open || !/^[A-Z]/.test(open.tag)) {
			out += src[i++];
			continue;
		}
		if (open.selfClose) {
			i = open.openEnd;
			continue;
		}
		const closeEnd = findClose(src, open.tag, open.openEnd);
		if (closeEnd < 0) {
			i = open.openEnd;
			continue;
		}
		let k = closeEnd - 1;
		while (k >= 0 && src[k] !== "<") k--;
		out += src.slice(open.openEnd, k);
		i = closeEnd;
	}
	return out;
}

function rewriteInternalLinks(src) {
	return src.replace(/\]\((\/[^)\s]+)\)/g, (full, target) => {
		if (target.endsWith(".llms.txt")) return full;
		if (target.startsWith("//")) return full;
		const hashIdx = target.indexOf("#");
		const filePart = hashIdx < 0 ? target : target.slice(0, hashIdx);
		const hashPart = hashIdx < 0 ? "" : target.slice(hashIdx);
		const clean = filePart.replace(/\/$/, "");
		return `](${clean}.llms.txt${hashPart})`;
	});
}

function transformMdxToMarkdown(src) {
	let out = src;
	out = stripImports(out);
	out = transformAISection(out);
	out = transformPromptExample(out);
	out = transformDetails(out);
	out = transformCapabilityGrid(out);
	out = transformSourceLink(out);
	out = transformConfigCode(out);
	out = transformConfigTabs(out);
	out = transformCallout(out);
	out = transformTabs(out);
	out = transformSteps(out);
	out = stripBareLeftoverComponents(out);
	out = rewriteInternalLinks(out);
	out = out.replace(/\n{3,}/g, "\n\n").trim();
	return out + "\n";
}

/* -------------------------------------------------------------------------- */
/* Output paths                                                                */
/* -------------------------------------------------------------------------- */

function relToLlmsTxtPath(rel) {
	const stripped = rel.replace(/\.(mdx|md)$/i, "");
	return path.join(PUBLIC_DIR, `${stripped}.llms.txt`);
}

function relToUrlPath(rel) {
	const stripped = rel.replace(/\.(mdx|md)$/i, "");
	if (stripped === "index") return "/";
	if (stripped.endsWith("/index")) return "/" + stripped.replace(/\/index$/, "");
	return "/" + stripped;
}

function relToLlmsUrlPath(rel) {
	// Maps an MDX file to its `.llms.txt` URL.
	// The companion file always lives at `<path>.llms.txt`, including for
	// `index.mdx` — its companion is at `/index.llms.txt`, not `.llms.txt`
	// (which would be a malformed URL).
	const stripped = rel.replace(/\.(mdx|md)$/i, "");
	return `/${stripped}.llms.txt`;
}

/* -------------------------------------------------------------------------- */
/* Main                                                                         */
/* -------------------------------------------------------------------------- */

/* -------------------------------------------------------------------------- */
/* Navigation tree from _meta.js                                                */
/* -------------------------------------------------------------------------- */

/**
 * Load a `_meta.js` file. They're CommonJS (`module.exports = { ... }`),
 * so we use `require` via createRequire.
 */
function loadMeta(metaPath) {
	try {
		// Clear cache so successive builds pick up edits.
		delete require.cache[require.resolve(metaPath)];
		return require(metaPath);
	} catch {
		return null;
	}
}

/**
 * Walk the `pages/` directory and build a hierarchical navigation tree that
 * mirrors what Nextra would render in the sidebar. Each node is
 * `{ key, title, kind, href?, children?, description? }`.
 * `kind` is one of: "separator", "page", "folder", "external".
 */
async function buildNavTree(dir, rel = "", entryByPath) {
	const meta = loadMeta(path.join(dir, "_meta.js"));
	const dirEntries = await fs.readdir(dir, { withFileTypes: true });
	const fileSet = new Set();
	for (const e of dirEntries) {
		if (e.name.startsWith("_") || e.name.startsWith(".")) continue;
		if (e.isFile() && (e.name.endsWith(".mdx") || e.name.endsWith(".md"))) {
			fileSet.add(e.name.replace(/\.(mdx|md)$/i, ""));
		} else if (e.isDirectory()) {
			fileSet.add(e.name);
		}
	}

	const orderedKeys = meta ? Object.keys(meta) : Array.from(fileSet).sort();
	const seen = new Set(orderedKeys);
	for (const k of fileSet) if (!seen.has(k)) orderedKeys.push(k);

	const nodes = [];

	for (const key of orderedKeys) {
		const metaEntry = meta?.[key];
		const isSeparator =
			metaEntry && typeof metaEntry === "object" && metaEntry.type === "separator";
		if (isSeparator) {
			nodes.push({
				key,
				title: metaEntry.title ?? key,
				kind: "separator",
			});
			continue;
		}

		// Could be a folder or a page
		const entryPath = path.join(dir, key);
		const mdxPath = `${entryPath}.mdx`;
		const mdPath = `${entryPath}.md`;
		const isFolder = (await fs.stat(entryPath).catch(() => null))?.isDirectory();
		const isPage =
			(await fs.stat(mdxPath).catch(() => null))?.isFile() ||
			(await fs.stat(mdPath).catch(() => null))?.isFile();

		const titleFromMeta =
			typeof metaEntry === "string"
				? metaEntry
				: metaEntry?.title;

		const relForEntry = rel ? `${rel}/${key}` : key;

		if (isFolder) {
			const subtree = await buildNavTree(entryPath, relForEntry, entryByPath);
			// Skip folders that contain no documentation pages (e.g.
			// `config/images/` which only holds asset PNGs).
			const hasPages = (function any(nodes) {
				return nodes.some(
					(n) => n.kind === "page" || (n.kind === "folder" && any(n.children ?? [])),
				);
			})(subtree);
			if (!hasPages) continue;
			nodes.push({
				key,
				title: titleFromMeta ?? toTitle(key),
				kind: "folder",
				// When the meta declares `display: "children"`, Nextra renders
				// the folder's children inline (no folder wrapper in the
				// sidebar). Mirror that here so the nav tree matches the UI.
				inlineChildren:
					typeof metaEntry === "object" && metaEntry?.display === "children",
				children: subtree,
			});
		} else if (isPage) {
			const entry = entryByPath.get(relForEntry);
			nodes.push({
				key,
				title: titleFromMeta ?? entry?.title ?? toTitle(key),
				kind: "page",
				href: entry?.llmsUrlPath
					? `${SITE_BASE_URL}${entry.llmsUrlPath}`
					: null,
				description: entry?.description,
			});
		} else if (metaEntry && typeof metaEntry === "object" && metaEntry.href) {
			nodes.push({
				key,
				title: titleFromMeta ?? toTitle(key),
				kind: "external",
				href: metaEntry.href.startsWith("http")
					? metaEntry.href
					: `${SITE_BASE_URL}${metaEntry.href}`,
			});
		}
	}

	return nodes;
}

function toTitle(key) {
	return key
		.replace(/-/g, " ")
		.replace(/^./, (c) => c.toUpperCase());
}

/**
 * Render the navigation tree as a hierarchical markdown list. Pages link to
 * their `.llms.txt`; folders become headings or nested bullets depending on
 * their depth.
 */
function renderNavTree(nodes, depth = 0) {
	const out = [];
	const indent = "  ".repeat(depth);

	for (const node of nodes) {
		if (node.kind === "separator") {
			// Render separators as a heading at depth 2-3 so the top section
			// reads like an outline rather than a flat list.
			out.push(`\n### ${node.title}\n`);
			continue;
		}
		if (node.kind === "external") {
			out.push(`${indent}- [${node.title}](${node.href})`);
			continue;
		}
		if (node.kind === "page") {
			const label = node.title || node.key;
			const linkLine = node.href
				? `${indent}- [${label}](${node.href})`
				: `${indent}- ${label}`;
			out.push(
				node.description
					? `${linkLine} — ${node.description}`
					: linkLine,
			);
			continue;
		}
		if (node.kind === "folder") {
			if (node.inlineChildren && node.children && node.children.length > 0) {
				// `display: "children"` — render the children directly at the
				// parent's depth, without a folder wrapper line. This mirrors
				// Nextra's sidebar behavior.
				out.push(renderNavTree(node.children, depth));
			} else {
				out.push(`${indent}- **${node.title}**`);
				if (node.children && node.children.length > 0) {
					out.push(renderNavTree(node.children, depth + 1));
				}
			}
		}
	}
	return out.join("\n");
}

/** Delete all previously generated *.llms.txt under public/ so removed or
 * moved pages don't leave stale machine-readable companions behind. */
async function cleanStaleLlmsTxt(dir) {
	const entries = await fs.readdir(dir, { withFileTypes: true }).catch(() => []);
	for (const entry of entries) {
		const abs = path.join(dir, entry.name);
		if (entry.isDirectory()) {
			await cleanStaleLlmsTxt(abs);
			const remaining = await fs.readdir(abs).catch(() => ["x"]);
			if (remaining.length === 0) await fs.rmdir(abs).catch(() => {});
		} else if (entry.isFile() && entry.name.endsWith(".llms.txt")) {
			await fs.unlink(abs).catch(() => {});
		}
	}
}

async function main() {
	await cleanStaleLlmsTxt(PUBLIC_DIR);
	const files = await walkMdx(PAGES_DIR);
	files.sort((a, b) => a.rel.localeCompare(b.rel));

	console.log(`[llms] processing ${files.length} MDX pages`);

	// First pass: render every page's .llms.txt and gather metadata
	const entries = [];
	const entryByPath = new Map();
	let indexBodyMarkdown = "";

	for (const { abs, rel } of files) {
		const raw = await fs.readFile(abs, "utf8");
		const { frontmatter, body } = splitFrontmatter(raw);
		const markdown = transformMdxToMarkdown(body);

		const urlPath = relToUrlPath(rel);
		const fullUrl = `${SITE_BASE_URL}${urlPath}`;
		const llmsUrlPath = relToLlmsUrlPath(rel);

		const titleMatch = body.match(/^#\s+(.+)$/m);
		const title = frontmatter.title || (titleMatch ? titleMatch[1].trim() : rel);
		const description = frontmatter.description?.replace(/\.\.\.$/, "").trim() ?? "";

		const header = [
			`# ${title}`,
			"",
			`> Source: ${fullUrl}`,
			description ? `> ${description}` : null,
			"> Format: machine-readable markdown export of the docs page above.",
			"> All collapsible AI sections are inlined and fully expanded.",
			"",
		]
			.filter((l) => l !== null)
			.join("\n");

		const entry = {
			rel,
			urlPath,
			fullUrl,
			llmsUrlPath,
			title,
			description,
			header,
			body: markdown,
		};
		entries.push(entry);
		entryByPath.set(rel.replace(/\.(mdx|md)$/i, ""), entry);

		// Capture the index page's transformed body so we can inline it into
		// the root llms.txt.
		if (rel === "index.mdx" || rel === "index.md") {
			indexBodyMarkdown = markdown;
		}
	}

	// Second pass: write every per-page .llms.txt, appending a "Navigation"
	// section with parent (up), children (down), and siblings (sideways) so an
	// agent can traverse the whole docs tree without returning to the root
	// index. The root /llms.txt is always linked as the global entry point.
	const stripExt = (rel) => rel.replace(/\.(mdx|md)$/i, "");
	const line = (e) =>
		`- [${e.title}](${SITE_BASE_URL}${e.llmsUrlPath})${e.description ? ` — ${e.description}` : ""}`;
	for (const entry of entries) {
		const dir = path.dirname(entry.rel); // "." for top-level pages
		const base = stripExt(entry.rel);

		// Parent: the page whose path equals this page's directory
		// (config/failsafe/hedge.mdx → config/failsafe.mdx). Top-level pages
		// have no parent page; the root llms.txt covers them.
		const parent =
			dir !== "." ? entries.find((e) => stripExt(e.rel) === dir) : null;
		// Children: pages living inside the directory named after this page
		// (config/failsafe.mdx → config/failsafe/*.mdx).
		const children = entries.filter(
			(e) => path.dirname(e.rel) === base,
		);
		const siblings = entries.filter(
			(e) => e !== entry && path.dirname(e.rel) === dir,
		);

		const nav = [
			"\n\n## Navigation (machine-readable surface)",
			"",
			`- Up: ${parent ? `[${parent.title}](${SITE_BASE_URL}${parent.llmsUrlPath})` : `[All pages index](${SITE_BASE_URL}/llms.txt)`}`,
			`- Root index of every page: [llms.txt](${SITE_BASE_URL}/llms.txt) · everything in one file: [llms-full.txt](${SITE_BASE_URL}/llms-full.txt)`,
		];
		if (children.length > 0) {
			nav.push("", "### Child pages", "", ...children.map(line));
		}
		if (siblings.length > 0) {
			nav.push("", "### Sibling pages", "", ...siblings.map(line));
		}

		const outPath = relToLlmsTxtPath(entry.rel);
		await fs.mkdir(path.dirname(outPath), { recursive: true });
		await fs.writeFile(
			outPath,
			`${entry.header}\n${entry.body}${nav.join("\n")}\n`,
			"utf8",
		);
	}

	// Build the hierarchical navigation tree from `_meta.js`.
	const navTree = await buildNavTree(PAGES_DIR, "", entryByPath);
	const navMarkdown = renderNavTree(navTree, 0);

	// Compose the root llms.txt: header + home-page content + navigation tree
	// + a flat "all pages" list (good for AI fan-out and search).
	const rootLines = [
		"# eRPC documentation — full reference",
		"",
		`> Source: ${SITE_BASE_URL}/`,
		"> This file is the AI-friendly entry point for the eRPC documentation.",
		"> It contains the home page content, the full navigation tree, and a",
		"> flat list of every page — each linked to its own machine-readable",
		"> companion at `<page>.llms.txt`. Append `.llms.txt` to any docs URL",
		"> to fetch that page's expanded markdown (all collapsible AI sections",
		"> inlined). Internal links inside `.llms.txt` files also point at",
		"> `.llms.txt` so an AI agent can crawl the entire reference without",
		"> leaving the machine-readable surface.",
		"",
		"---",
		"",
		"## Home page",
		"",
		indexBodyMarkdown.trim(),
		"",
		"---",
		"",
		"## Navigation",
		"",
		"The same hierarchy a reader sees in the docs sidebar — every leaf",
		"links to its `.llms.txt` companion.",
		"",
		navMarkdown,
		"",
		"---",
		"",
		"## Every page (flat list)",
		"",
		"Convenient when an agent wants to iterate over every page rather than",
		"follow the hierarchy:",
		"",
	];
	for (const entry of entries) {
		const titleLink = `[${entry.title}](${SITE_BASE_URL}${entry.llmsUrlPath})`;
		rootLines.push(
			entry.description
				? `- ${titleLink} — ${entry.description}`
				: `- ${titleLink}`,
		);
	}
	rootLines.push("");

	await fs.writeFile(
		path.join(PUBLIC_DIR, "llms.txt"),
		rootLines.join("\n"),
		"utf8",
	);

	// llms-full.txt — every page concatenated into one fetch for agents with
	// large context windows. Pages are separated by a heading banner.
	const fullLines = [
		"# eRPC documentation — llms-full.txt",
		"",
		`> Source: ${SITE_BASE_URL}/`,
		"> Every docs page concatenated into a single machine-readable file.",
		"> For the navigable index, fetch /llms.txt instead.",
		"",
	];
	for (const entry of entries) {
		fullLines.push(
			"",
			"---",
			"",
			`<!-- page: ${entry.urlPath} -->`,
			"",
			entry.header,
			entry.body.trim(),
		);
	}
	await fs.writeFile(
		path.join(PUBLIC_DIR, "llms-full.txt"),
		fullLines.join("\n") + "\n",
		"utf8",
	);

	console.log(
		`[llms] wrote ${files.length} per-page .llms.txt files + root index + llms-full.txt at ${PUBLIC_DIR}`,
	);
}

main().catch((err) => {
	console.error("[llms] failed:", err);
	process.exit(1);
});
