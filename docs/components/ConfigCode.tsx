import React from "react";
import { Highlight, themes, type Language } from "prism-react-renderer";

type HighlightRenderProps = {
	className: string;
	style: React.CSSProperties;
	tokens: Array<Array<{ types: string[]; content: string; empty?: boolean }>>;
	getLineProps: (input: { line: HighlightRenderProps["tokens"][number] }) => {
		className?: string;
		style?: React.CSSProperties;
		[key: string]: unknown;
	};
	getTokenProps: (input: { token: HighlightRenderProps["tokens"][number][number] }) => {
		className?: string;
		style?: React.CSSProperties;
		children?: string;
		[key: string]: unknown;
	};
};

export interface ConfigCodeProps {
	/** Filename label, e.g. "erpc.yaml" or "erpc.ts". */
	filename?: string;
	/**
	 * Hierarchical breadcrumb showing WHERE this snippet lives in the full
	 * config tree, e.g. "projects > networks > failsafe.retry". Rendered
	 * above the code block so readers know exactly where to paste it.
	 */
	path?: string;
	/** Code language token (yaml, typescript, bash, json, etc.). */
	language: Language | string;
	/**
	 * Lines (1-indexed) to keep at full opacity. Everything else is dimmed.
	 * Accepts a comma-separated list with optional ranges, e.g. "6-8,12,15-17".
	 * Omit to render at full opacity throughout.
	 */
	focus?: string;
	/** Show line numbers in the gutter. Default false (keep snippets minimal). */
	showLineNumbers?: boolean;
	/**
	 * Code body. Two ways to pass it (use whichever is more ergonomic):
	 *  • As a `code` prop (preferred when wiring from another React component).
	 *  • As JSX children (preferred when authoring inline in MDX).
	 */
	code?: string;
	children?: React.ReactNode;
}

function parseFocus(focus: string | undefined): Set<number> | null {
	if (!focus) return null;
	const result = new Set<number>();
	for (const part of focus.split(",")) {
		const range = part.trim();
		if (!range) continue;
		if (range.includes("-")) {
			const [startStr, endStr] = range.split("-");
			const start = Number.parseInt(startStr, 10);
			const end = Number.parseInt(endStr, 10);
			if (Number.isFinite(start) && Number.isFinite(end)) {
				for (let i = Math.min(start, end); i <= Math.max(start, end); i++) {
					result.add(i);
				}
			}
		} else {
			const n = Number.parseInt(range, 10);
			if (Number.isFinite(n)) result.add(n);
		}
	}
	return result;
}

function childrenToString(node: React.ReactNode): string {
	if (typeof node === "string") return node;
	if (typeof node === "number" || typeof node === "boolean") return String(node);
	if (Array.isArray(node)) return node.map(childrenToString).join("");
	if (node && typeof node === "object" && "props" in node) {
		const props = (node as { props?: { children?: React.ReactNode } }).props;
		return childrenToString(props?.children);
	}
	return "";
}

function renderPathBreadcrumb(path: string): React.ReactNode {
	const segments = path
		.split(/\s*[>›\/]\s*/)
		.map((s) => s.trim())
		.filter(Boolean);
	return segments.map((segment, i) => (
		<React.Fragment key={`${segment}-${i}`}>
			<span className="cv-cc-path-seg">{segment}</span>
			{i < segments.length - 1 && (
				<span className="cv-cc-path-sep" aria-hidden="true">
					{" › "}
				</span>
			)}
		</React.Fragment>
	));
}

export function ConfigCode({
	filename,
	path,
	language,
	focus,
	showLineNumbers,
	code: codeProp,
	children,
}: ConfigCodeProps) {
	const focusSet = parseFocus(focus);
	const code = (codeProp ?? childrenToString(children)).replace(/^\n+|\n+$/g, "");

	return (
		<div
			className="cv-cc"
			data-component="config-code"
			data-language={language}
			data-path={path ?? undefined}
			data-filename={filename ?? undefined}
			data-focus={focus ?? undefined}
		>
			{(path || filename) && (
				<div className="cv-cc-header">
					{path && <div className="cv-cc-path">{renderPathBreadcrumb(path)}</div>}
					{filename && <div className="cv-cc-filename">{filename}</div>}
				</div>
			)}
			{React.createElement(
				Highlight as unknown as React.ComponentType<{
					code: string;
					language: Language;
					theme: typeof themes.vsDark;
					children: (props: HighlightRenderProps) => React.ReactElement;
				}>,
				{
					code,
					language: language as Language,
					theme: themes.vsDark,
					children: ({ className, style, tokens, getLineProps, getTokenProps }) => (
						<pre className={`cv-cc-pre ${className}`} style={style}>
							<code>
								{tokens.map((line, i) => {
									const lineNum = i + 1;
									const focused = focusSet ? focusSet.has(lineNum) : true;
									const lineProps = getLineProps({ line });
									return (
										<span
											{...lineProps}
											key={`line-${lineNum}`}
											className={`cv-cc-line ${focused ? "cv-cc-focus" : "cv-cc-dim"} ${lineProps.className ?? ""}`}
											data-line={lineNum}
										>
											{showLineNumbers && (
												<span className="cv-cc-linenum" aria-hidden="true">
													{lineNum}
												</span>
											)}
											<span className="cv-cc-linecontent">
												{line.map((token, j) => (
													<span
														key={`tok-${lineNum}-${j}`}
														{...getTokenProps({ token })}
													/>
												))}
											</span>
										</span>
									);
								})}
							</code>
						</pre>
					),
				},
			)}
		</div>
	);
}

export default ConfigCode;
