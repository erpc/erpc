// Resizer.jsx — drag handles for resizable splits.
// Exposes: window.Splitter — a thin gutter that calls onResize(delta) on drag.

function Splitter({ orientation = "v", onResize, className = "" }) {
  const isCol = orientation === "v";
  function onMouseDown(e) {
    e.preventDefault();
    let lastX = e.clientX;
    let lastY = e.clientY;
    document.body.classList.add("dragging-split");
    document.body.classList.add(isCol ? "col-resize" : "row-resize");
    const handle = e.currentTarget;
    handle.classList.add("dragging");
    function onMove(ev) {
      const dx = ev.clientX - lastX;
      const dy = ev.clientY - lastY;
      lastX = ev.clientX;
      lastY = ev.clientY;
      if (dx === 0 && dy === 0) return;
      onResize(isCol ? dx : dy);
    }
    function onUp() {
      document.body.classList.remove("dragging-split");
      document.body.classList.remove(isCol ? "col-resize" : "row-resize");
      handle.classList.remove("dragging");
      window.removeEventListener("mousemove", onMove);
      window.removeEventListener("mouseup", onUp);
    }
    window.addEventListener("mousemove", onMove);
    window.addEventListener("mouseup", onUp);
  }
  return (
    <div
      className={`split ${isCol ? "split-v" : "split-h"} ${className}`}
      onMouseDown={onMouseDown}
    />
  );
}

window.Splitter = Splitter;
