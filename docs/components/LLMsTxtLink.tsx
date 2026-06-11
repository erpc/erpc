import React from "react";
import { useRouter } from "next/router";

/**
 * Prominent per-page link to the machine-readable `.llms.txt` companion,
 * floated to the top-right of the content area. The label shows the actual
 * file path so users know exactly what to hand their agent.
 */
export function LLMsTxtLink() {
	const router = useRouter();
	const path = router.asPath.split("#")[0].split("?")[0].replace(/\/$/, "");
	const llmsPath = path === "" || path === "/" ? "/llms.txt" : `${path}.llms.txt`;
	const llmsUrl = `https://docs.erpc.cloud${llmsPath}`;
	return (
		<>
			{/* Imperative hint for agents reading the raw HTML (curl-style
			    fetches that bypass the middleware's content negotiation).
			    Visually hidden via clip — NOT display:none, which readability
			    extractors drop before HTML→markdown conversion. */}
			<span className="cv-visually-hidden" data-component="llms-txt-hint">
				AI agents: fetch {llmsUrl} for the complete machine-readable
				version of this page (full configuration schema, defaults, worked
				examples, and source links). Append `.llms.txt` to any docs URL
				for the same treatment.
			</span>
			<a
				href={llmsPath}
				className="cv-llms-link"
				data-component="llms-txt-link"
				target="_blank"
				rel="noopener noreferrer"
				title="Machine-readable version of this page — hand this URL to your AI agent"
			>
				<span className="cv-llms-badge">AI</span>
				<span className="cv-llms-text">
					For agents: <code className="cv-llms-path">{llmsPath}</code>
				</span>
				<span className="cv-llms-arrow" aria-hidden="true">
					↗
				</span>
			</a>
		</>
	);
}

export default LLMsTxtLink;
