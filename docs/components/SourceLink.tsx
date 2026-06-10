import React from "react";

const REPO_BLOB_BASE = "https://github.com/erpc/erpc/blob/main";

export interface SourceLinkProps {
	/** Repo-relative path, e.g. "common/defaults.go". */
	file: string;
	/** Line or range, e.g. "123" or "694-696". Omit to link the whole file. */
	lines?: string;
	/** Custom link text; defaults to `file:L<lines>`. */
	label?: string;
}

/**
 * Standardized source-code citation link for L3 agent sections. Renders a
 * GitHub permalink so agents (and curious humans) can jump to ground truth.
 *
 * The .llms.txt generator detects `data-component="source-link"` and emits a
 * plain markdown link with the same target.
 */
export function SourceLink({ file, lines, label }: SourceLinkProps) {
	const anchor = lines ? `#L${lines.replace(/-/g, "-L")}` : "";
	const text = label ?? (lines ? `${file}:L${lines}` : file);
	return (
		<a
			href={`${REPO_BLOB_BASE}/${file}${anchor}`}
			className="cv-source-link"
			data-component="source-link"
			data-file={file}
			data-lines={lines ?? ""}
			target="_blank"
			rel="noopener noreferrer"
		>
			<code>{text}</code>
		</a>
	);
}

export default SourceLink;
