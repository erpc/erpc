import { NextResponse, type NextRequest } from "next/server";

/**
 * Content negotiation for AI agents.
 *
 * Every docs page has a machine-readable twin at `<path>.llms.txt` (generated
 * by `scripts/build-llms.mjs` into `public/`). A request is redirected to that
 * twin when either:
 *
 *   1. the `Accept` header explicitly lists `text/markdown` — Claude Code
 *      sends `Accept: text/markdown, text/html, *​/*` on every web fetch, and
 *      no browser ever sends that token; or
 *   2. the `User-Agent` matches a known AI fetcher (fallback for agents that
 *      don't do Accept-based negotiation yet).
 *
 * Why a 302 redirect instead of silently rewriting the response body:
 *   - the `.llms.txt` URL shows up in the agent's transcript, teaching it the
 *     scheme — and every `.llms.txt` file links onward to other `.llms.txt`
 *     pages, so the agent stays on the markdown surface for the whole session;
 *   - humans and agents always see the same content at the same URL, which
 *     keeps caching honest and debugging sane.
 *
 * Why 302 and not 301: clients cache 301s per-URL unconditionally, so a
 * single misclassified client would be stuck on the text version forever.
 */

// Living list — extend as new agent fetchers publish their User-Agent
// strings. Deliberately excludes search-engine crawlers (Googlebot, Bingbot)
// so the HTML pages stay the indexed surface.
const AI_FETCHER_UA =
	/Claude-User|Claude-SearchBot|ClaudeBot|claude-code|GPTBot|ChatGPT-User|OAI-SearchBot|codex|Perplexity-User|PerplexityBot|Meta-ExternalAgent|Meta-ExternalFetcher|MistralAI-User|cohere-ai|DuckAssistBot|Devin|Bytespider|Amazonbot/i;

export function middleware(req: NextRequest) {
	if (req.method !== "GET" && req.method !== "HEAD") {
		return;
	}

	const accept = req.headers.get("accept") ?? "";
	const userAgent = req.headers.get("user-agent") ?? "";

	const wantsMarkdown = /\btext\/markdown\b/i.test(accept);
	if (!wantsMarkdown && !AI_FETCHER_UA.test(userAgent)) {
		return;
	}

	const pathname = req.nextUrl.pathname.replace(/\/+$/, "");
	const url = req.nextUrl.clone();
	url.pathname = pathname === "" ? "/llms.txt" : `${pathname}.llms.txt`;
	url.search = "";

	const res = NextResponse.redirect(url, 302);
	// The response depends on both negotiation signals — keep shared caches
	// from serving an agent's redirect to a browser (or vice versa).
	res.headers.set("Vary", "Accept, User-Agent");
	return res;
}

export const config = {
	// Only run on page routes: skip Next internals, API routes, the OG-image
	// route, and anything with a file extension (assets, *.llms.txt itself,
	// robots.txt, sitemap.xml, …).
	matcher: ["/((?!_next/|api/|og$|.*\\..*).*)"],
};
