import React from "react";
import { useRouter } from "next/router";

/**
 * Floating button rendered on every docs page that links to the current
 * page's machine-readable `.llms.txt` companion. Lets a reader open or copy
 * the expanded, AI-ready version of the page they're on with one click.
 */
export function LLMsTxtLink() {
	const router = useRouter();
	const path = router.asPath.split("#")[0].split("?")[0].replace(/\/$/, "");
	const llmsPath = path === "" || path === "/" ? "/llms.txt" : `${path}.llms.txt`;
	return (
		<a
			href={llmsPath}
			className="cv-llms-link"
			data-component="llms-txt-link"
			target="_blank"
			rel="noopener noreferrer"
			title="Open AI-friendly markdown version of this page"
		>
			<span className="cv-llms-badge">AI</span>
			<span className="cv-llms-text">Open as plain markdown for AI</span>
			<span className="cv-llms-arrow" aria-hidden="true">
				↗
			</span>
		</a>
	);
}

export default LLMsTxtLink;
