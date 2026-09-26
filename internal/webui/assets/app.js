const state = {
	torrents: [],
	expanded: new Set(),
	filter: "all",
	search: "",
	sortKey: "name",
	sortDir: "asc",
	stats: null,
	speedHistory: [],
	sessionStatsTimer: null,
	listTimer: null,
	expandTimer: null,
	finishedAt: new Map(),
	pendingAdds: [],
	pendingAddSeq: 0,
	pendingTimer: null,
};

const PENDING_STORAGE_KEY = "bittrench-pending-adds";

// Persist pending-add labels to sessionStorage so they survive a page reload.
// Note: the in-flight fetch is *not* re-issued after reload (we can't re-attach
// to the original request), but the backend keeps processing the POST. We
// restore the label so the user sees "Adding… (restored)" rather than an empty
// list, and the periodic torrent refresh will eventually pick up the result.
function persistPendingAdds() {
	const serializable = state.pendingAdds.map((p) => ({
		label: p.label,
		startedAt: p.startedAt,
		stale: p.stale || false,
	}));
	try {
		sessionStorage.setItem(PENDING_STORAGE_KEY, JSON.stringify(serializable));
	} catch (_) {
		/* sessionStorage may be unavailable */
	}
}

function restorePendingAdds() {
	try {
		const raw = sessionStorage.getItem(PENDING_STORAGE_KEY);
		if (!raw) return;
		// Drop state we no longer care about - no fetch to abort.
		sessionStorage.removeItem(PENDING_STORAGE_KEY);
		const restoredAdds = JSON.parse(raw);
		if (!Array.isArray(restoredAdds) || restoredAdds.length === 0) return;
		for (const p of restoredAdds) {
			const id = `pending-${++state.pendingAddSeq}`;
			state.pendingAdds.push({
				id,
				label: p.label,
				error: null,
				startedAt: p.startedAt || Date.now(),
				stale: true,
			});
		}
	} catch (_) {
		/* parse errors ignored */
	}
}

const $ = (id) => document.getElementById(id);
const tbody = $("torrent-tbody");
const versionEl = $("version");

const API = "/api/v1";

async function api(path, opts) {
	const r = await fetch(API + path, opts);
	if (!r.ok) throw new Error(`${r.status} ${r.statusText}`);
	const ct = r.headers.get("content-type") || "";
	return ct.includes("application/json") ? r.json() : r.text();
}

function fmtBytes(b) {
	if (b == null || Number.isNaN(b)) return "-";
	const units = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];
	let i = 0;
	let v = b;
	while (v >= 1024 && i < units.length - 1) {
		v /= 1024;
		i++;
	}
	return `${i === 0 ? v : v.toFixed(v < 10 ? 2 : 1)} ${units[i]}`;
}
function fmtBytesPerSec(b) {
	return `${fmtBytes(b)}/s`;
}
function fmtDuration(secs) {
	if (secs == null || !Number.isFinite(secs)) return "-";
	secs = Math.max(0, Math.round(secs));
	const d = Math.floor(secs / 86400);
	const h = Math.floor((secs % 86400) / 3600);
	const m = Math.floor((secs % 3600) / 60);
	const s = secs % 60;
	if (d > 0) return `${d}d ${h}h`;
	if (h > 0) return `${h}h ${m}m`;
	if (m > 0) return `${m}m ${s}s`;
	return `${s}s`;
}

async function loadVersion() {
	// The native API exposes no version endpoint; the title is static and the
	// banner reports whether the daemon is answering at all.
	try {
		await api("/stats");
		versionEl.textContent = "connected";
		versionEl.classList.remove("error");
	} catch (_e) {
		versionEl.textContent = "disconnected";
		versionEl.classList.add("error");
	}
}

async function refreshList() {
	try {
		const r = await api("/torrents");
		state.torrents = r.torrents || [];
		updateTable();
	} catch (e) {
		if (!tbody.querySelector(".empty")) {
			tbody.innerHTML = `<tr><td colspan="9" class="empty">Failed to load torrents: ${escapeHtml(e.message)}</td></tr>`;
		}
	}
}

function escapeHtml(s) {
	return String(s).replace(/[&<>"']/g, (c) => {
		switch (c) {
			case "&":
				return "&amp;";
			case "<":
				return "&lt;";
			case ">":
				return "&gt;";
			case '"':
				return "&quot;";
			case "'":
				return "&#39;";
			default:
				return c;
		}
	});
}

function torrentName(t) {
	return t.name || t.info_hash || "(unknown)";
}
function torrentId(t) {
	return String(t.id != null ? t.id : t.info_hash);
}
function torrentSize(t) {
	return t.total_bytes || 0;
}
function torrentProgressPct(t) {
	if (!t.total_bytes) return 0;
	return (100 * t.completed_bytes) / t.total_bytes;
}
function torrentDownSpeed(t) {
	// Already bytes per second: the engine samples the library's cumulative
	// counters, so there is no MiB/s conversion to undo any more.
	return t.download_rate || 0;
}
function torrentUpSpeed(t) {
	return t.upload_rate || 0;
}
function torrentState(t) {
	return t.state || "initializing";
}
function torrentEta(t) {
	// The API reports -1 for unknown.
	return t.eta_seconds >= 0 ? t.eta_seconds : null;
}
function torrentPeers(t) {
	return `${t.seeders || 0}/${t.peers || 0}`;
}
function torrentUploadedBytes(t) {
	return t.uploaded_bytes || 0;
}
function torrentRatio(t) {
	return t.ratio || 0;
}
function torrentFinished(t) {
	return t.has_metadata && t.state === "seeding";
}
function fmtRatio(r) {
	return r.toFixed(2);
}

function statusDot(state) {
	const cls =
		state === "downloading"
			? "live"
			: state === "seeding"
				? "finished"
				: state === "stopped"
					? "paused"
					: state === "error"
						? "error"
						: "initializing";
	return `<span class="status-dot ${cls}" title="${escapeHtml(state)}"></span>`;
}

function progressClass(t) {
	const s = torrentState(t);
	if (s === "error") return "error";
	if (s === "stopped") return "paused";
	if (torrentFinished(t)) return "done";
	return "";
}

function renderTable() {
	const rows = filterAndSort(state.torrents);
	tbody.innerHTML = "";
	if (state.torrents.length === 0 && state.pendingAdds.length === 0) {
		tbody.innerHTML =
			'<tr><td colspan="9" class="empty">No torrents. Click “+ Add torrent” to add one.</td></tr>';
		return;
	}
	if (rows.length === 0 && state.pendingAdds.length === 0) {
		tbody.innerHTML =
			'<tr><td colspan="9" class="empty">No torrents match the current filter.</td></tr>';
		return;
	}
	const frag = document.createDocumentFragment();
	for (const p of state.pendingAdds) frag.append(buildPendingRow(p));
	for (const t of rows) {
		const id = torrentId(t);
		const tr = buildRow(t);
		frag.append(tr);
		if (state.expanded.has(id)) {
			const expandTr = document.createElement("tr");
			expandTr.className = "expand-row";
			expandTr.innerHTML = `<td colspan="9"><div class="expand" id="expand-${cssEscape(id)}"></div></td>`;
			frag.append(expandTr);
		}
	}
	tbody.append(frag);
	for (const t of rows) {
		if (state.expanded.has(torrentId(t))) renderExpand(t);
	}
}

function buildPendingRow(p) {
	const tr = document.createElement("tr");
	tr.className = "pending-row";
	tr.dataset.pendingId = p.id;
	const elapsed = p.startedAt
		? Math.max(0, Math.floor((Date.now() - p.startedAt) / 1000))
		: 0;
	const elapsedLabel = p.stale
		? "- still pending (restored after reload)"
		: `- adding… ${elapsed}s`;
	if (p.error) {
		tr.innerHTML =
			`<td class="cell-status"><span class="status-dot error" title="error"></span></td>` +
			`<td class="name">${escapeHtml(p.label)} <span style="color:var(--bar-error)">- ${escapeHtml(p.error)}</span></td>` +
			`<td colspan="6" style="text-align:right"><button class="icon" data-pending-dismiss="${escapeHtml(p.id)}" title="Dismiss">✕</button></td>`;
	} else {
		tr.innerHTML =
			`<td class="cell-status"><span class="spinner" title="adding"></span></td>` +
			`<td class="name">${escapeHtml(p.label)} <span class="pending-elapsed" style="color:var(--muted)">${escapeHtml(elapsedLabel)}</span></td>` +
			`<td colspan="7" style="text-align:right"><button class="icon" data-pending-dismiss="${escapeHtml(p.id)}" title="Cancel add">✕</button></td>`;
	}
	return tr;
}

function buildRow(t) {
	const id = torrentId(t);
	const pct = torrentProgressPct(t);
	const cls = progressClass(t);
	const dl = torrentDownSpeed(t);
	const ul = torrentUpSpeed(t);
	const tr = document.createElement("tr");
	tr.className = "torrent-row";
	if (state.expanded.has(id)) tr.classList.add("selected");
	if (torrentState(t) === "paused") tr.classList.add("paused");
	tr.dataset.id = id;
	tr.innerHTML =
		`<td class="cell-status">${statusDot(torrentState(t))}</td>` +
		`<td class="name">${escapeHtml(torrentName(t))}</td>` +
		`<td><div class="progress-cell"><div class="progress ${cls}"><div class="bar" style="width:${pct.toFixed(2)}%"></div></div>` +
		`<span class="pct">${pct.toFixed(2)}%</span></div></td>` +
		`<td class="num">${fmtBytes(torrentSize(t))}</td>` +
		`<td class="num">${dl > 0 ? fmtBytesPerSec(dl) : "-"}</td>` +
		`<td class="num">${ul > 0 ? fmtBytesPerSec(ul) : "-"}</td>` +
		`<td class="num">${escapeHtml(torrentPeers(t))}</td>` +
		`<td class="num">${fmtDuration(torrentEta(t))}</td>` +
		`<td><button class="icon" data-act="pause" title="Pause/Resume">⏯</button>` +
		`<button class="icon" data-act="forget" title="Forget torrent, keep downloaded files">⏏</button>` +
		`<button class="icon danger" data-act="delete" title="Delete torrent and its downloaded files">🗑</button></td>`;
	return tr;
}

function updateRowInPlace(tr, t) {
	const id = torrentId(t);
	const tState = torrentState(t);
	tr.classList.toggle("selected", state.expanded.has(id));
	tr.classList.toggle("paused", tState === "paused");
	if (tr.dataset.id !== id) tr.dataset.id = id;
	const cells = tr.children;
	cells[0].innerHTML = statusDot(tState);
	cells[1].textContent = torrentName(t);
	const pct = torrentProgressPct(t);
	const cell = cells[2];
	const progressDiv = cell.querySelector(".progress");
	progressDiv.className = `progress ${progressClass(t)}`;
	progressDiv.firstElementChild.style.width = `${pct.toFixed(2)}%`;
	cell.querySelector(".pct").textContent = `${pct.toFixed(2)}%`;
	const dl = torrentDownSpeed(t);
	const ul = torrentUpSpeed(t);
	cells[3].textContent = fmtBytes(torrentSize(t));
	cells[4].textContent = dl > 0 ? fmtBytesPerSec(dl) : "-";
	cells[5].textContent = ul > 0 ? fmtBytesPerSec(ul) : "-";
	cells[6].textContent = torrentPeers(t);
	cells[7].textContent = fmtDuration(torrentEta(t));
}

function updateTable() {
	const rows = filterAndSort(state.torrents);
	if (
		state.torrents.length === 0 &&
		rows.length === 0 &&
		state.pendingAdds.length === 0
	) {
		const msg = "No torrents. Click “+ Add torrent” to add one.";
		const empty = tbody.querySelector(".empty");
		if (!empty || empty.textContent !== msg) {
			tbody.innerHTML = `<tr><td colspan="9" class="empty">${msg}</td></tr>`;
		}
		return;
	}
	if (
		state.torrents.length === 0 &&
		rows.length === 0 &&
		state.pendingAdds.length > 0
	) {
		// Show only pending rows
		tbody.innerHTML = "";
		const frag = document.createDocumentFragment();
		for (const p of state.pendingAdds) frag.append(buildPendingRow(p));
		tbody.append(frag);
		return;
	}
	if (rows.length === 0 && state.pendingAdds.length === 0) {
		const empty = tbody.querySelector(".empty");
		if (!empty) {
			tbody.innerHTML =
				'<tr><td colspan="9" class="empty">No torrents match the current filter.</tr>';
		}
		return;
	}
	if (tbody.querySelector(".empty")) tbody.innerHTML = "";
	// Remove old pending rows, then prepend fresh ones.
	for (const tr of Array.from(tbody.querySelectorAll("tr.pending-row")))
		tr.remove();
	const pendingFrag = document.createDocumentFragment();
	for (const p of state.pendingAdds) pendingFrag.append(buildPendingRow(p));
	if (pendingFrag.childNodes.length > 0) tbody.prepend(pendingFrag);
	const existing = new Map();
	for (const tr of Array.from(tbody.querySelectorAll("tr.torrent-row"))) {
		existing.set(tr.dataset.id, tr);
	}
	const frag = document.createDocumentFragment();
	for (const t of rows) {
		const id = torrentId(t);
		const wantExpand = state.expanded.has(id);
		let tr = existing.get(id);
		let expandTr = null;
		if (tr) {
			existing.delete(id);
			const next = tr.nextElementSibling;
			if (next?.classList?.contains("expand-row")) expandTr = next;
			updateRowInPlace(tr, t);
		} else {
			tr = buildRow(t);
		}
		if (wantExpand && !expandTr) {
			expandTr = document.createElement("tr");
			expandTr.className = "expand-row";
			expandTr.innerHTML = `<td colspan="9"><div class="expand" id="expand-${cssEscape(id)}"></div></td>`;
		} else if (!wantExpand && expandTr) {
			expandTr.remove();
			expandTr = null;
		}
		frag.append(tr);
		if (expandTr) frag.append(expandTr);
	}
	for (const [, tr] of existing) {
		const next = tr.nextElementSibling;
		if (next?.classList?.contains("expand-row")) next.remove();
		tr.remove();
	}
	tbody.append(frag);
	for (const t of rows) {
		const id = torrentId(t);
		if (state.expanded.has(id)) {
			const el = document.getElementById(`expand-${cssEscape(id)}`);
			if (el && el.childElementCount === 0) renderExpand(t);
		}
	}
}

function cssEscape(s) {
	return String(s).replace(
		/[^a-zA-Z0-9_-]/g,
		(c) => `_${c.charCodeAt(0).toString(16)}`,
	);
}

function filterAndSort(list) {
	let r = list.slice();
	if (state.search) {
		const q = state.search.toLowerCase();
		r = r.filter((t) => torrentName(t).toLowerCase().includes(q));
	}
	if (state.filter !== "all") {
		r = r.filter((t) => {
			const s = torrentState(t);
			const fin = t.stats?.finished;
			if (state.filter === "live") return s === "live" && !fin;
			if (state.filter === "paused") return s === "paused";
			if (state.filter === "finished") return s !== "error" && fin;
			if (state.filter === "error") return s === "error";
			return true;
		});
	}
	const key = state.sortKey;
	const dir = state.sortDir === "asc" ? 1 : -1;
	r.sort((a, b) => {
		let av;
		let bv;
		if (key === "name") {
			av = torrentName(a);
			bv = torrentName(b);
			return ((av > bv) - (av < bv)) * dir;
		}
		if (key === "size") {
			av = torrentSize(a);
			bv = torrentSize(b);
		} else if (key === "progress") {
			av = torrentProgressPct(a);
			bv = torrentProgressPct(b);
		} else if (key === "speed") {
			av = torrentDownSpeed(a);
			bv = torrentDownSpeed(b);
		}
		return ((av > bv) - (av < bv)) * dir;
	});
	return r;
}

async function renderExpand(t) {
	const id = torrentId(t);
	const host = $(`expand-${cssEscape(id)}`);
	if (!host) return;
	host.innerHTML =
		`<div class="subgrid">` +
		`<div><h3>Peers</h3><table class="peers" id="peers-${cssEscape(id)}"><thead><tr>` +
		`<th>Address</th><th class="num">↓ Bytes</th><th class="num">↓ Rate</th><th class="num">Pieces</th>` +
		`<th>Client</th><th>Source</th></tr></thead><tbody><tr><td colspan="6">Loading…</td></tr></tbody></table></div>` +
		`<div><h3>Pieces</h3><canvas class="pieces" id="pieces-${cssEscape(id)}" width="640" height="80"></canvas></div>` +
		`<div><h3>Files</h3><div id="files-${cssEscape(id)}">Loading…</div></div>` +
		`<div><h3>Info</h3><table class="info" id="info-${cssEscape(id)}"></table></div>` +
		`</div>`;
	loadPeers(id);
	loadPieces(id);
	loadFiles(id);
	updateInfoSection(t);
}

async function refreshExpanded() {
	const promises = [];
	for (const id of state.expanded) {
		promises.push(loadPeers(id));
		promises.push(loadPieces(id));
		const t = state.torrents.find((x) => torrentId(x) === id);
		if (t) updateInfoSection(t);
	}
	await Promise.all(promises);
}

async function loadPeers(id) {
	const body = $(`peers-${cssEscape(id)}`);
	if (!body) return;
	const tb = body.querySelector("tbody");
	const t = state.torrents.find((x) => torrentId(x) === id);
	const st = t ? torrentState(t) : "";
	if (st === "stopped" || st === "error") {
		const msg =
			st === "stopped"
				? "Stopped - no peers while not running."
				: "Errored - no peers.";
		tb.innerHTML = `<tr><td colspan="6" style="color:var(--muted);text-align:center">${msg}</td></tr>`;
		return;
	}
	try {
		const r = await api(`/torrents/${encodeURIComponent(id)}/peers`);
		const all = r.peers || [];
		// Show peers that are live or have actually transferred something;
		// a swarm's worth of idle connections is noise.
		const interesting = all.filter(
			(p) => (p.bytes_read || 0) > 0 || (p.download_rate || 0) > 0,
		);
		const shown = interesting.length > 0 ? interesting : all;
		if (shown.length === 0) {
			tb.innerHTML = '<tr><td colspan="6">No peers.</td></tr>';
			return;
		}
		shown.sort((a, b) => (b.bytes_read || 0) - (a.bytes_read || 0));
		let html = "";
		for (const p of shown) {
			html +=
				`<tr><td>${escapeHtml(p.addr || "")}</td>` +
				`<td class="num">${fmtBytes(p.bytes_read || 0)}</td>` +
				`<td class="num">${fmtBytesPerSec(p.download_rate || 0)}</td>` +
				`<td class="num">${p.pieces_have || 0}</td>` +
				`<td>${escapeHtml(p.client || "")}</td>` +
				`<td>${escapeHtml(p.source || "")}</td></tr>`;
		}
		const hidden = all.length - shown.length;
		if (hidden > 0) {
			html += `<tr><td colspan="6" style="color:var(--muted);text-align:center">${hidden} idle peer(s) hidden.</td></tr>`;
		}
		tb.innerHTML = html;
	} catch (e) {
		tb.innerHTML = `<tr><td colspan="6">Peers unavailable: ${escapeHtml(e.message)}</td></tr>`;
	}
}

async function loadPieces(id) {
	const canvas = $(`pieces-${cssEscape(id)}`);
	if (!canvas) return;
	try {
		// A base64 bitfield plus an explicit piece count. The count is what
		// matters: the last byte is padded, and without knowing where the
		// pieces stop the padding bits read as missing ones.
		const r = await api(`/torrents/${encodeURIComponent(id)}/pieces`);
		drawPieces(canvas, decodeBitfield(r.bitfield || "", r.count || 0));
	} catch (e) {
		const ctx = canvas.getContext("2d");
		ctx.fillStyle = getComputedStyle(document.body).color;
		ctx.fillText(`Pieces unavailable: ${e.message}`, 8, 18);
	}
}

// decodeBitfield turns base64 into a "0"/"1" string of exactly `count` bits,
// least significant bit first within each byte, which is how the engine packs
// them. The count is what stops the last byte's padding from being drawn.
function decodeBitfield(b64, count) {
	if (!b64 || count <= 0) return "";
	let raw;
	try {
		raw = atob(b64);
	} catch (_) {
		return "";
	}
	let bits = "";
	for (let i = 0; i < count; i++) {
		const byte = raw.charCodeAt(i >> 3) || 0;
		bits += (byte >> (i & 7)) & 1 ? "1" : "0";
	}
	return bits;
}

function drawPieces(canvas, bits) {
	// Size canvas to displayed pixels to keep things sharp: match the CSS
	// rendered width (which may be < 640 due to max-width:100%) and compute
	// the required height from the piece count.
	const rect = canvas.getBoundingClientRect();
	if (rect.width > 0) {
		canvas.width = Math.round(rect.width);
	}
	const W = canvas.width;
	const cell = 4;
	const cols = Math.floor(W / cell);
	if (!bits.length || cols === 0) {
		canvas.height = 80;
		const c = canvas.getContext("2d");
		c.fillStyle =
			getComputedStyle(document.body).getPropertyValue("--canvas-bg") ||
			"#f9fafb";
		c.fillRect(0, 0, canvas.width, canvas.height);
		c.fillStyle = getComputedStyle(document.body).color;
		c.fillText("no piece data", 8, 18);
		return;
	}
	const rows = Math.ceil(bits.length / cols);
	canvas.height = Math.max(20, rows * cell);
	const H = canvas.height;
	const c = canvas.getContext("2d");
	const bg =
		getComputedStyle(document.body).getPropertyValue("--canvas-bg") ||
		"#f9fafb";
	const fg = getComputedStyle(document.body).color;
	const have =
		getComputedStyle(document.body).getPropertyValue("--piece-have") || fg;
	const missing =
		getComputedStyle(document.body).getPropertyValue("--piece-missing") ||
		"#d97706";
	c.fillStyle = bg;
	c.fillRect(0, 0, W, H);
	for (let i = 0; i < bits.length; i++) {
		const x = (i % cols) * cell;
		const y = Math.floor(i / cols) * cell;
		if (y >= H) break;
		c.fillStyle = bits[i] === "1" ? have : missing;
		c.fillRect(x, y, cell, cell);
	}
}

async function loadFiles(id) {
	const host = $(`files-${cssEscape(id)}`);
	if (!host) return;
	try {
		const det = await api(`/torrents/${encodeURIComponent(id)}/files`);
		const files = det.files || [];
		if (!files.length) {
			host.textContent = "No file metadata yet.";
			return;
		}
		const onlyFiles = new Set(files.flatMap((f, i) => (f.selected ? [i] : [])));
		let html =
			'<table class="files"><thead><tr><th>✓</th><th>Name</th><th class="num">Size</th></tr></thead><tbody>';
		files.forEach((f, i) => {
			const checked = onlyFiles.has(i) ? " checked" : "";
			html +=
				`<tr><td><input type='checkbox'${checked} data-fi='${i}'></td>` +
				`<td>${escapeHtml(f.path)}</td>` +
				`<td class="num">${fmtBytes(f.length)}</td></tr>`;
		});
		html += "</tbody></table>";
		host.innerHTML = html;
		host.querySelectorAll("input[type=checkbox]").forEach((cb) => {
			cb.addEventListener("change", () => onFileToggle(id, host));
		});
	} catch (e) {
		host.textContent = `Files unavailable: ${e.message}`;
	}
}

function updateInfoSection(t) {
	const id = torrentId(t);
	const tbl = $(`info-${cssEscape(id)}`);
	if (!tbl) return;
	const uploaded = torrentUploadedBytes(t);
	const total = torrentSize(t);
	const ratio = torrentRatio(t);
	const tState = torrentState(t);
	const dl = torrentDownSpeed(t);
	const ul = torrentUpSpeed(t);
	const pct = torrentProgressPct(t);
	// The daemon records when a torrent finished and persists it, so seed time
	// survives a restart rather than restarting with the browser tab.
	const seedSecs = t.finished_at
		? Math.floor(Date.now() / 1000 - t.finished_at)
		: null;
	tbl.innerHTML =
		`<tr><th>State</th><td>${escapeHtml(tState)}</td></tr>` +
		`<tr><th>Progress</th><td>${pct.toFixed(2)}%</td></tr>` +
		`<tr><th>Size</th><td>${fmtBytes(total)}</td></tr>` +
		`<tr><th>Uploaded</th><td>${fmtBytes(uploaded)}</td></tr>` +
		`<tr><th>Ratio</th><td>${fmtRatio(ratio)}</td></tr>` +
		`<tr><th>↓ Speed</th><td>${dl > 0 ? fmtBytesPerSec(dl) : "-"}</td></tr>` +
		`<tr><th>↑ Speed</th><td>${ul > 0 ? fmtBytesPerSec(ul) : "-"}</td></tr>` +
		`<tr><th>ETA</th><td>${fmtDuration(torrentEta(t))}</td></tr>` +
		`<tr><th>Seed time</th><td>${seedSecs !== null ? fmtDuration(seedSecs) : "-"}</td></tr>`;
}

async function onFileToggle(id, host) {
	const checked = Array.from(
		host.querySelectorAll("input[type=checkbox]:checked"),
	).map((cb) => parseInt(cb.dataset.fi, 10));
	try {
		await api(`/torrents/${encodeURIComponent(id)}/files`, {
			method: "POST",
			headers: { "Content-Type": "application/json" },
			body: JSON.stringify({ selected: checked }),
		});
	} catch (e) {
		alert(`Failed to update file selection: ${e.message}`);
		loadFiles(id);
	}
}

async function actionPause(id) {
	try {
		await api(`/torrents/${encodeURIComponent(id)}/stop`, { method: "POST" });
		refreshList();
	} catch (e) {
		alert(`Pause failed: ${e.message}`);
	}
}
async function actionStart(id) {
	try {
		await api(`/torrents/${encodeURIComponent(id)}/start`, { method: "POST" });
		refreshList();
	} catch (e) {
		alert(`Resume failed: ${e.message}`);
	}
}
async function actionDelete(id) {
	if (!confirm("Delete this torrent AND its downloaded files?")) return;
	try {
		await api(`/torrents/${encodeURIComponent(id)}?delete_data=true`, {
			method: "DELETE",
		});
		state.expanded.delete(id);
		refreshList();
	} catch (e) {
		alert(`Delete failed: ${e.message}`);
	}
}
async function actionForget(id) {
	if (!confirm("Forget this torrent (keep downloaded files on disk)?")) return;
	try {
		await api(`/torrents/${encodeURIComponent(id)}`, { method: "DELETE" });
		state.expanded.delete(id);
		refreshList();
	} catch (e) {
		alert(`Forget failed: ${e.message}`);
	}
}

tbody.addEventListener("click", (ev) => {
	const dismissBtn = ev.target.closest("button[data-pending-dismiss]");
	if (dismissBtn) {
		ev.stopPropagation();
		removePending(dismissBtn.dataset.pendingDismiss);
		return;
	}
	const btn = ev.target.closest("button[data-act]");
	if (btn) {
		ev.stopPropagation();
		const tr = ev.target.closest("tr");
		const id = tr.dataset.id;
		if (btn.dataset.act === "pause") {
			const t = state.torrents.find((x) => torrentId(x) === id);
			if (t && torrentState(t) === "paused") actionStart(id);
			else actionPause(id);
		} else if (btn.dataset.act === "delete") {
			actionDelete(id);
		} else if (btn.dataset.act === "forget") {
			actionForget(id);
		}
		return;
	}
	const tr = ev.target.closest("tr.torrent-row");
	if (!tr) return;
	const id = tr.dataset.id;
	if (state.expanded.has(id)) state.expanded.delete(id);
	else state.expanded.add(id);
	renderTable();
});

document.querySelectorAll("th[data-sort]").forEach((th) => {
	th.addEventListener("click", () => {
		const key = th.dataset.sort;
		if (state.sortKey === key) {
			state.sortDir = state.sortDir === "asc" ? "desc" : "asc";
		} else {
			state.sortKey = key;
			state.sortDir = "asc";
		}
		document.querySelectorAll("th[data-sort]").forEach((t) => {
			t.classList.remove("sorted-asc", "sorted-desc");
		});
		th.classList.add(state.sortDir === "asc" ? "sorted-asc" : "sorted-desc");
		renderTable();
	});
});

$("search").addEventListener("input", (e) => {
	state.search = e.target.value.trim();
	renderTable();
});

document.querySelectorAll(".filter-btn").forEach((btn) => {
	btn.addEventListener("click", () => {
		document.querySelectorAll(".filter-btn").forEach((b) => {
			b.classList.remove("active");
		});
		btn.classList.add("active");
		state.filter = btn.dataset.filter;
		renderTable();
	});
});

$("sort-key").addEventListener("change", (e) => {
	state.sortKey = e.target.value;
	renderTable();
});
$("sort-dir").addEventListener("change", (e) => {
	state.sortDir = e.target.value;
	renderTable();
});

const addDialog = $("add-dialog");
$("add-btn").addEventListener("click", () => {
	$("add-magnet").value = "";
	$("add-folder").value = "";
	$("add-file").value = "";
	setAddStatus("", false);
	addDialog.showModal();
});
addDialog.querySelector("form").addEventListener("submit", (ev) => {
	ev.preventDefault();
	const magnet = $("add-magnet").value.trim();
	const folder = $("add-folder").value.trim();
	const fileInput = $("add-file");
	if (fileInput.files.length > 0) {
		uploadFile(fileInput.files[0], folder);
		addDialog.close();
	} else if (magnet) {
		uploadMagnet(magnet, folder);
		addDialog.close();
	} else {
		setAddStatus("Enter a magnet/URL or pick a .torrent file.", false, true);
	}
});
$("add-cancel").addEventListener("click", () => {
	addDialog.close();
});

async function uploadMagnet(value, folder) {
	const id = `pending-${++state.pendingAddSeq}`;
	const label = value.length > 50 ? `${value.slice(0, 50)}…` : value;
	const ac = new AbortController();
	state.pendingAdds.push({
		id,
		label,
		error: null,
		startedAt: Date.now(),
		abort: ac,
	});
	persistPendingAdds();
	renderTable();
	try {
		await api("/torrents", {
			method: "POST",
			headers: { "Content-Type": "application/json" },
			body: JSON.stringify({ source: value, download_dir: folder || "" }),
			signal: ac.signal,
		});
		removePending(id);
		refreshList();
	} catch (e) {
		if (e.name === "AbortError") {
			removePending(id);
		} else {
			updatePending(id, e.message);
		}
	}
}

async function uploadFile(file, folder) {
	const metainfo = await fileToBase64(file);
	const id = `pending-${++state.pendingAddSeq}`;
	const ac = new AbortController();
	state.pendingAdds.push({
		id,
		label: file.name,
		error: null,
		startedAt: Date.now(),
		abort: ac,
	});
	persistPendingAdds();
	renderTable();
	try {
		await api("/torrents", {
			method: "POST",
			headers: { "Content-Type": "application/json" },
			body: JSON.stringify({ metainfo, download_dir: folder || "" }),
			signal: ac.signal,
		});
		removePending(id);
		refreshList();
	} catch (e) {
		if (e.name === "AbortError") {
			removePending(id);
		} else {
			updatePending(id, e.message);
		}
	}
}

// fileToBase64 reads a .torrent into the base64 the API expects. Chunked so a
// large file does not blow the argument limit of String.fromCharCode.
async function fileToBase64(file) {
	const bytes = new Uint8Array(await file.arrayBuffer());
	let binary = "";
	const chunk = 0x8000;
	for (let i = 0; i < bytes.length; i += chunk) {
		binary += String.fromCharCode.apply(null, bytes.subarray(i, i + chunk));
	}
	return btoa(binary);
}

function removePending(id) {
	const p = state.pendingAdds.find((x) => x.id === id);
	if (p?.abort) {
		try {
			p.abort.abort();
		} catch (_) {
			/* already aborted */
		}
	}
	state.pendingAdds = state.pendingAdds.filter((p) => p.id !== id);
	persistPendingAdds();
	renderTable();
}

function updatePending(id, error) {
	const p = state.pendingAdds.find((x) => x.id === id);
	if (p) {
		p.error = error;
		// Drop the AbortController reference once the request actually settled.
		p.abort = null;
	}
	persistPendingAdds();
	renderTable();
}

function setAddStatus(msg, _busy, isError) {
	const el = $("add-status");
	if (isError) {
		el.textContent = msg;
		el.classList.add("error");
	} else {
		el.textContent = "";
		el.classList.remove("error");
	}
}

async function refreshSessionStats() {
	try {
		const s = await api("/stats");
		state.stats = s;
		const dl = s.download_rate || 0;
		const ul = s.upload_rate || 0;
		$("dl-speed").textContent = fmtBytesPerSec(dl);
		$("ul-speed").textContent = fmtBytesPerSec(ul);
		$("fetched").textContent = fmtBytes(s.completed_bytes || 0);
		$("uploaded").textContent = fmtBytes(
			state.torrents.reduce((n, t) => n + torrentUploadedBytes(t), 0),
		);
		$("peer-count").textContent = String(
			state.torrents.reduce((n, t) => n + (t.peers || 0), 0),
		);
		$("uptime").textContent =
			`${s.active || 0} active / ${s.paused || 0} stopped`;
		state.speedHistory.push({ dl, ul });
		if (state.speedHistory.length > 120) state.speedHistory.shift();
		drawSparkline();
	} catch (_e) {
		/* ignore */
	}
}

function drawSparkline() {
	const canvas = $("sparkline");
	const ctx = canvas.getContext("2d");
	const W = canvas.width;
	const H = canvas.height;
	const bg =
		getComputedStyle(document.body).getPropertyValue("--canvas-bg") ||
		"#f9fafb";
	const grid =
		getComputedStyle(document.body).getPropertyValue("--canvas-grid") ||
		"#e5e7eb";
	const downColor =
		getComputedStyle(document.body).getPropertyValue("--canvas-line-down") ||
		"#2563eb";
	const upColor =
		getComputedStyle(document.body).getPropertyValue("--canvas-line-up") ||
		"#10b981";
	ctx.fillStyle = bg;
	ctx.fillRect(0, 0, W, H);
	const hist = state.speedHistory;
	if (hist.length < 2) {
		ctx.fillStyle = grid;
		ctx.fillText("-", 8, 22);
		return;
	}
	let max = 1;
	for (const p of hist) {
		if (p.dl > max) max = p.dl;
		if (p.ul > max) max = p.ul;
	}
	const x = (i) => (i / (hist.length - 1)) * (W - 2) + 1;
	const y = (v) => H - 2 - (v / max) * (H - 4);
	function line(getter, color) {
		ctx.strokeStyle = color;
		ctx.lineWidth = 1;
		ctx.beginPath();
		hist.forEach((p, i) => {
			const px = x(i);
			const py = y(getter(p));
			if (i === 0) ctx.moveTo(px, py);
			else ctx.lineTo(px, py);
		});
		ctx.stroke();
	}
	line((p) => p.dl, downColor);
	line((p) => p.ul, upColor);
}

(async function init() {
	restorePendingAdds();
	await loadVersion();
	await refreshList();
	await refreshSessionStats();
	state.listTimer = setInterval(refreshList, 5000);
	state.sessionStatsTimer = setInterval(refreshSessionStats, 1000);
	state.expandTimer = setInterval(refreshExpanded, 2000);
	state.pendingTimer = setInterval(refreshPendingElapsed, 1000);
})();

// Tick the elapsed-time label on pending-rows once per second without doing a
// full table rebuild (avoids disturbing the rest of the table).
function refreshPendingElapsed() {
	if (state.pendingAdds.length === 0) return;
	const rows = Array.from(tbody.querySelectorAll("tr.pending-row"));
	let changed = false;
	for (const tr of rows) {
		const p = state.pendingAdds.find((x) => x.id === tr.dataset.pendingId);
		if (!p || p.error || !p.startedAt) continue;
		const span = tr.querySelector(".pending-elapsed");
		if (!span) continue;
		const elapsed = Math.max(0, Math.floor((Date.now() - p.startedAt) / 1000));
		const label = p.stale
			? "- still pending (restored after reload)"
			: `- adding… ${elapsed}s`;
		if (span.textContent !== label) {
			span.textContent = label;
			changed = true;
		}
	}
	return changed;
}
