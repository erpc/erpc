import React from "react";

export interface PromptExampleProps {
	/** 1-based example number, rendered as "Prompt Example #N:". */
	n: number;
	/** Short goal phrase shown after the number. */
	title: string;
	/** The exact prompt text users copy into their agent session. */
	prompt: string;
	/** Open by default (use for the first example on a page). */
	defaultOpen?: boolean;
}

/**
 * Collapsible, copy-first example prompt for AI agent sessions. Styled like a
 * plain text editor: monospace body, hairline borders, no color noise — the
 * Copy button is the hero.
 *
 * The .llms.txt generator detects `data-component="prompt-example"` and emits
 * the title plus a fenced text block.
 */
export function PromptExample({
	n,
	title,
	prompt,
	defaultOpen = false,
}: PromptExampleProps) {
	const [copied, setCopied] = React.useState(false);
	const text = prompt.trim();

	const copy = (e: React.MouseEvent) => {
		e.preventDefault();
		navigator.clipboard?.writeText(text).then(() => {
			setCopied(true);
			window.setTimeout(() => setCopied(false), 1500);
		});
	};

	return (
		<details
			className="cv-prompt"
			data-component="prompt-example"
			data-n={n}
			data-title={title}
			open={defaultOpen}
		>
			<summary className="cv-prompt-summary">
				<span className="cv-prompt-caret" aria-hidden="true" />
				<span className="cv-prompt-label">
					Prompt Example #{n}: <span className="cv-prompt-title">{title}</span>
				</span>
			</summary>
			<div className="cv-prompt-body">
				<button
					type="button"
					className="cv-prompt-copy"
					onClick={copy}
					aria-label="Copy prompt to clipboard"
				>
					{copied ? "Copied" : "Copy"}
				</button>
				<pre className="cv-prompt-pre">{text}</pre>
			</div>
		</details>
	);
}

export default PromptExample;
