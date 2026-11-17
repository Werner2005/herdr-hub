import React from "react";

export function ExpandableEntry({ name, badge, summary, children }) {
 return <details className="workspace-card expandable-entry">
  <summary className="entry-summary">
   <span className="entry-overview"><span className="entry-heading"><span className="entry-name">{name}</span>{badge&&<span className="date-pill">{badge}</span>}</span><span className="entry-description">{summary}</span></span>
   <span className="entry-toggle" aria-hidden="true"><span className="entry-expand">Expand</span><span className="entry-collapse">Collapse</span></span>
  </summary>
  <div className="entry-details">{children}</div>
 </details>;
}
