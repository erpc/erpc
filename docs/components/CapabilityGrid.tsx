import React from "react";
import Link from "next/link";

export interface CapabilityGridItem {
	title: string;
	href: string;
	summary: string;
	/** Optional emoji rendered before the title. */
	icon?: string;
}

export interface CapabilityGridProps {
	items: CapabilityGridItem[];
	/** CSS grid column count. Default 3. */
	columns?: 2 | 3 | 4;
}

/**
 * "Capabilities at a glance" card grid used on capability overview pages
 * (/routing, /resilience, /caching, /security, /evm, /architecture).
 *
 * The .llms.txt generator detects `data-component="capability-grid"` and the
 * `data-items` JSON payload, emitting a markdown bullet list whose links point
 * at each child page's `.llms.txt` companion.
 */
export function CapabilityGrid({ items, columns = 3 }: CapabilityGridProps) {
	return (
		<div
			className="cv-capgrid"
			data-component="capability-grid"
			data-items={JSON.stringify(
				items.map(({ title, href, summary }) => ({ title, href, summary })),
			)}
			style={{ ["--cv-capgrid-cols" as string]: columns }}
		>
			{items.map((item) => (
				<Link key={item.href} href={item.href} className="cv-capgrid-card">
					<span className="cv-capgrid-title">
						{item.icon && (
							<span className="cv-capgrid-icon" aria-hidden="true">
								{item.icon}{" "}
							</span>
						)}
						{item.title}
					</span>
					<span className="cv-capgrid-summary">{item.summary}</span>
				</Link>
			))}
		</div>
	);
}

export default CapabilityGrid;
