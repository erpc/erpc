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
 * The .llms.txt generator detects `data-component="ai-section"` and inlines
 * the body fully expanded, so AI-side consumers see all of it by default.
 */
export function AISection({
	title = "Copy for your AI assistant",
	hint = "Expand for every option, default, and edge case — or copy this entire section into your AI assistant.",
	defaultOpen = false,
	children,
}: AISectionProps) {
	return (
		<details
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
