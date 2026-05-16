import React, { useEffect, useRef } from "react";
import { HERO_SVG_HTML, initHero } from "./hero-diagram.internals";

export interface HeroDiagramProps {
	/** Max width in CSS units. Default `1200px`. */
	maxWidth?: string;
}

/**
 * Hero diagram, fully inlined into the React tree (no iframe, no static
 * asset). Styles live in `docs/styles/hero-diagram.css` (imported globally
 * from `_app.tsx`); SVG markup + animation script live in
 * `./hero-diagram.internals.ts`.
 *
 * Interaction surface:
 *   • Click an upstream's vertical pill to cordon / un-cordon it.
 *   • Toggle the cache pill near the eRPC wordmark.
 *   • Click a failsafe lane / box to focus on it.
 *   • Drag the slider next to each upstream to adjust its latency.
 *   • Hit "Reset" once any state has been mutated.
 */
export function HeroDiagram({ maxWidth = "1200px" }: HeroDiagramProps) {
	const rootRef = useRef<HTMLDivElement>(null);

	useEffect(() => {
		if (!rootRef.current) return;
		return initHero(rootRef.current);
	}, []);

	return (
		<div
			ref={rootRef}
			className="cv-hero-root"
			data-component="hero-diagram"
			style={{ width: "100%", maxWidth, margin: "1.25rem auto 1.5rem" }}
		>
			<div className="hero" role="img" aria-label="eRPC hero diagram">
				<div dangerouslySetInnerHTML={{ __html: HERO_SVG_HTML }} />
			</div>
		</div>
	);
}

export default HeroDiagram;
