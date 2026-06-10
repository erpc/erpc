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
	return (
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
	);
}

export default LLMsTxtLink;
