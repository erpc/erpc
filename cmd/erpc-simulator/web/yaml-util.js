// ============================================================
// YAML utilities — tiny tokenizer + structural editor.
// Not a full YAML parser. Handles the eRPC config shape:
//   key: value, key: { inline }, indented blocks, list items.
// Enough to drive the knob mirror and the diagnostics line markers.
// ============================================================
(function () {
  "use strict";

  // ------- highlight tokens (line by line) ----------------------------------
  function highlightLine(line) {
    // returns HTML string with token spans
    // 1. comment
    const ci = line.indexOf("#");
    let codePart = line, commentPart = "";
    if (ci >= 0 && !inString(line, ci)) {
      codePart = line.slice(0, ci);
      commentPart = line.slice(ci);
    }
    let out = "";
    // 2. key: value pattern
    const kvm = codePart.match(/^(\s*-?\s*)([A-Za-z_][\w\-]*)(\s*:)(.*)$/);
    if (kvm) {
      out += esc(kvm[1]);
      out += `<span class="tk-key">${esc(kvm[2])}</span>`;
      out += `<span class="tk-punc">${esc(kvm[3])}</span>`;
      out += highlightValue(kvm[4]);
    } else {
      // list item or raw
      const li = codePart.match(/^(\s*)(-)(\s+)(.*)$/);
      if (li) {
        out += esc(li[1]);
        out += `<span class="tk-punc">${esc(li[2])}</span>`;
        out += esc(li[3]);
        out += highlightValue(li[4]);
      } else {
        out += highlightValue(codePart);
      }
    }
    if (commentPart) out += `<span class="tk-com">${esc(commentPart)}</span>`;
    return out || "&nbsp;";
  }

  function highlightValue(s) {
    if (!s) return "";
    let v = s;
    // strings (quoted)
    v = v.replace(/("(?:\\.|[^"\\])*")/g, '<span class="tk-str">$1</span>');
    // numbers + units (5s, 250ms, 12.5)
    v = v.replace(/(?<![\w])(\d+(?:\.\d+)?(?:ms|s|m|h|kb|mb|gb)?)\b/gi,
      '<span class="tk-num">$1</span>');
    // booleans + null
    v = v.replace(/\b(true|false|null|~)\b/g, '<span class="tk-bool">$1</span>');
    return v;
  }
  function inString(line, idx) {
    // naive: true if hash is inside a quoted region
    let q = false;
    for (let i = 0; i < idx; i++) {
      const c = line[i];
      if (c === '"' && line[i - 1] !== "\\") q = !q;
    }
    return q;
  }
  function esc(s) {
    return s.replace(/[&<>]/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;" }[c]));
  }

  // ------- find line for a YAML path ----------------------------------------
  // path: e.g. ["projects",0,"failsafe",0,"retry","maxAttempts"]
  // very limited: returns first line whose indent + key chain matches.
  function findLineForPath(yaml, path) {
    const lines = yaml.split("\n");
    let depth = -1;
    let cursor = 0;
    let stack = [];
    for (let i = 0; i < lines.length; i++) {
      const ln = lines[i];
      const t = ln.replace(/^\s*/, "");
      const indent = ln.length - t.length;
      if (!t || t.startsWith("#")) continue;
      // pop stack
      while (stack.length > 0 && stack[stack.length - 1].indent >= indent) stack.pop();
      const m = t.match(/^(-\s+)?([A-Za-z_][\w-]*)\s*:/);
      if (!m) continue;
      const key = m[2];
      stack.push({ indent, key });
      const pathHere = stack.map(s => s.key);
      // compare ignoring list indices in path
      if (pathHere.join(".") === path.filter(p => typeof p !== "number").join(".")) {
        return i + 1; // 1-indexed
      }
    }
    return null;
  }

  // ------- find diagnostics in YAML ------------------------------------------
  function diagnose(yaml) {
    const diags = [];
    const lines = yaml.split("\n");
    let braces = 0;
    lines.forEach((ln, idx) => {
      // legacy fields → migration warning
      if (/^\s*scoreMetricsWindowSize\s*:/.test(ln))
        diags.push({ line: idx + 1, severity: "warn", msg: "scoreMetricsWindowSize moved to metrics.scoreWindowSize (auto-migrated)" });
      if (/^\s*routingStrategy\s*:/.test(ln))
        diags.push({ line: idx + 1, severity: "warn", msg: "routingStrategy → selectionPolicy (deprecated, auto-migrated)" });
      // tabs
      if (/^\t/.test(ln))
        diags.push({ line: idx + 1, severity: "error", msg: "tabs not allowed; use spaces" });
      // odd indent
      const m = ln.match(/^( +)/);
      if (m && m[1].length % 2 === 1 && ln.trim().length > 0)
        diags.push({ line: idx + 1, severity: "warn", msg: "odd indentation" });
    });
    return diags;
  }

  // ------- structural editor: set value at YAML path -------------------------
  // Replaces the value of `key: oldVal` at the location implied by `path`.
  // If the key isn't found, no-op.
  function setValue(yaml, path, newVal) {
    const lineNum = findLineForPath(yaml, path);
    if (!lineNum) return yaml;
    const lines = yaml.split("\n");
    const ln = lines[lineNum - 1];
    const m = ln.match(/^(\s*-?\s*[A-Za-z_][\w-]*\s*:\s*)(.*)$/);
    if (!m) return yaml;
    let v = String(newVal);
    if (typeof newVal === "boolean") v = newVal ? "true" : "false";
    lines[lineNum - 1] = m[1] + v;
    return lines.join("\n");
  }

  window.YAMLU = { highlightLine, diagnose, findLineForPath, setValue };
})();
