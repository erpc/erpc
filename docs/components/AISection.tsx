import React from "react";

export interface AISectionProps {
	/** Headline shown in the collapsed summary. */
	title?: string;
	/** Optional secondary line shown in the collapsed summary. */
	hint?: string;
	/**
	 * When true, render expanded by default (rare — usually keep collapsed so
	 * the page stays scannable for humans).
	 */
	defaultOpen?: boolean;
	children: React.ReactNode;
}

/**
 * Collapsible "For AI" panel containing the comprehensive, exhaustive
 * reference for the surrounding feature — every flag, edge case, example,
 * and nuance. Humans skim the page above; when they need detail they expand
 * this section (or copy it into an AI assistant).
 *
 * The headings inside the panel are real MDX headings, so Nextra lists them
 * in the right-hand TOC. Since the browser cannot scroll to an anchor hidden
 * inside a closed <details>, this component auto-opens itself whenever the
 * URL hash targets an element it contains (on load and on every hash change),
 * then re-scrolls to the target.
 *
 * The .llms.txt generator detects `data-component="ai-section"` and inlines
 * the body fully expanded, so AI-side consumers see all of it by default.
 */
export function AISection({
	title = "Copy for your AI assistant",
	hint = "Expand for every option, default, and edge case — or copy this entire section into your AI assistant.",
	defaultOpen = false,
	children,
}: AISectionProps) {
	const ref = React.useRef<HTMLDetailsElement>(null);

	React.useEffect(() => {
		const openIfHashInside = () => {
			const el = ref.current;
			if (!el) return;
			const hash = window.location.hash;
			if (!hash || hash.length < 2) return;
			let target: Element | null = null;
			try {
				target = document.getElementById(decodeURIComponent(hash.slice(1)));
			} catch {
				return;
			}
			if (target && el.contains(target) && !el.open) {
				el.open = true;
				requestAnimationFrame(() => {
					target?.scrollIntoView();
				});
			}
		};
		openIfHashInside();
		window.addEventListener("hashchange", openIfHashInside);
		// TOC links use pushState-less anchor clicks; same-hash re-clicks don't
		// fire hashchange, so also catch anchor clicks targeting our content.
		const onClick = (e: MouseEvent) => {
			const a = (e.target as Element | null)?.closest?.('a[href^="#"]');
			if (!a) return;
			const id = decodeURIComponent((a.getAttribute("href") ?? "").slice(1));
			const el = ref.current;
			if (!el || !id) return;
			const target = document.getElementById(id);
			if (target && el.contains(target) && !el.open) {
				el.open = true;
				requestAnimationFrame(() => {
					target.scrollIntoView();
				});
			}
		};
		document.addEventListener("click", onClick);
		return () => {
			window.removeEventListener("hashchange", openIfHashInside);
			document.removeEventListener("click", onClick);
		};
	}, []);

	return (
		<details
			ref={ref}
			className="cv-ai"
			data-component="ai-section"
			data-title={title}
			open={defaultOpen}
		>
			<summary className="cv-ai-summary">
				<span className="cv-ai-badge" aria-hidden="true">
					AI
				</span>
				<span className="cv-ai-text">
					<span className="cv-ai-title">{title}</span>
					{hint && <span className="cv-ai-hint">{hint}</span>}
				</span>
				<span className="cv-ai-caret" aria-hidden="true" />
			</summary>
			<div className="cv-ai-body">{children}</div>
		</details>
	);
}

export default AISection;
