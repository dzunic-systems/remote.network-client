/**
 * A small, dependency-free markdown renderer for ASSISTANT chat text.
 *
 * The contract is deliberately narrow and the first rule is security, not
 * fidelity. The model's output quotes corpus text and fetched web pages — both
 * treated as data-never-instructions by the agent's system prompt — so the ONLY
 * safe pipeline is escape-then-format: the whole source is HTML-escaped before
 * any transform runs, and the renderer emits nothing but its own tags around
 * pieces that already went through that escape. No raw model text ever reaches
 * innerHTML.
 *
 * Coverage is the subset the model actually uses and this UI was asked for:
 * headings, bold/italic, inline + fenced code, lists, links, blockquotes and
 * GFM tables. Anything else renders as escaped plain text — for untrusted
 * input, a missing feature is honest and a wrong escape is not.
 */

const ESCAPE = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };

function escapeHtml(s) {
	return s.replace(/[&<>"']/g, (c) => ESCAPE[c]);
}

// inline renders one line's inline markdown. It runs on ALREADY-ESCAPED text,
// so every piece it emits is inert. Code spans are protected first so `**` or
// `*` inside them are never mistaken for emphasis; links are emitted with
// rel=noopener noreferrer so an external page can never reach back to the
// opener, and only http(s) targets become links at all.
function inline(s) {
	s = s.replace(/`([^`]+)`/g, (_m, code) => '<code>' + code + '</code>');
	s = s.replace(/\[([^\]]+)\]\((https?:\/\/[^)\s]+)\)/g, (_m, label, url) => {
		// url is already-escaped text, so it carries no literal quote; it goes
		// into the attribute exactly as escaped.
		return '<a href="' + url + '" rel="noopener noreferrer" target="_blank">' + label + '</a>';
	});
	s = s.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
	s = s.replace(/\*([^*\s][^*]*)\*/g, '<em>$1</em>');
	s = s.replace(/_([^_\s][^_]*)_/g, '<em>$1</em>');
	return s;
}

// splitCells splits one GFM table row on the pipe, stripping the leading and
// trailing delimiters a row is usually written with.
function splitCells(row) {
	let r = row.trim();
	if (r.startsWith('|')) r = r.slice(1);
	if (r.endsWith('|')) r = r.slice(0, -1);
	return r.split('|').map((c) => c.trim());
}

function isDelimiterRow(line) {
	// A GFM separator row: pipes around cells of dashes (and optional colons for
	// alignment), nothing else.
	return /^\|?[\s:|-]+\|?$/.test(line.trim()) && /-/.test(line);
}

export function renderMarkdown(src) {
	if (!src) return '';
	const lines = escapeHtml(src).replace(/\r\n?/g, '\n').split('\n');
	const out = [];
	let i = 0;

	while (i < lines.length) {
		const line = lines[i];
		const t = line.trim();

		if (t === '') {
			i++;
			continue;
		}

		// Fenced code: collect verbatim until the closing fence. The content was
		// escaped with the whole source, so it is inert; it is also NOT inline-
		// processed — code stays exactly as written.
		if (/^```/.test(t)) {
			const buf = [];
			i++;
			while (i < lines.length && !/^```/.test(lines[i].trim())) {
				buf.push(lines[i]);
				i++;
			}
			i++; // closing fence
			out.push('<pre><code>' + buf.join('\n') + '</code></pre>');
			continue;
		}

		// GFM table: a pipe row followed by a delimiter row.
		if (/^\|/.test(t) && i + 1 < lines.length && isDelimiterRow(lines[i + 1])) {
			const start = i;
			const rows = [];
			while (i < lines.length && /^\|/.test(lines[i].trim())) {
				rows.push(splitCells(lines[i]));
				i++;
			}
			// rows[1] is the separator; keep the header and the data rows. A
			// delimiter without a leading pipe (| a | b |\n---|---) is not
			// collected, so the "table" may hold only the header — and that
			// must not VANISH. Emit what was read as a paragraph: unstyled pipes
			// are an annoyance, vanished rows are a wrong answer.
			if (rows.length < 2) {
				const frag = lines.slice(start, i).map((l) => inline(l.trim())).join(' ');
				out.push('<p>' + frag + '</p>');
				continue;
			}
			const head = rows[0];
			const cells = (r) => {
				const cols = head.map((_, c) => (r[c] ?? ''));
				return cols.map((c) => '<td>' + inline(c) + '</td>').join('');
			};
			const thead = '<thead><tr>' + head.map((c) => '<th>' + inline(c) + '</th>').join('') + '</tr></thead>';
			const tbody = '<tbody>' + rows.slice(2).map((r) => '<tr>' + cells(r) + '</tr>').join('') + '</tbody>';
			// The wrapper owns horizontal scroll: a wide table must not push
			// the chat column sideways.
			// The expand button opens the table in the side window at full width
			// (Message.svelte delegates the click); inside that window it is hidden.
			out.push(
				'<div class="table-wrap"><button type="button" class="table-expand" title="Open this table wide" aria-label="Open this table wide">⤢</button><table>' +
					thead +
					tbody +
					'</table></div>'
			);
			continue;
		}

		// Headings.
		const h = t.match(/^(#{1,6})\s+(.*)$/);
		if (h) {
			out.push('<h' + h[1].length + '>' + inline(h[2]) + '</h' + h[1].length + '>');
			i++;
			continue;
		}

		// Horizontal rule.
		if (/^(-{3,}|\*{3,})$/.test(t)) {
			out.push('<hr>');
			i++;
			continue;
		}

		// Blockquote: consecutive quoted lines fold into one block. The `>`
		// marker is detected in its ESCAPED form (`&gt;`): the whole source was
		// escaped before parsing, so a literal `>` became `&gt;` and a literal
		// `&gt;` became `&amp;gt;` (which does not match here).
		if (/^&gt;/.test(t)) {
			const buf = [];
			while (i < lines.length && /^&gt;/.test(lines[i].trim())) {
				buf.push(inline(lines[i].trim().replace(/^&gt;\s?/, '')));
				i++;
			}
			out.push('<blockquote>' + buf.join('<br>') + '</blockquote>');
			continue;
		}

		// Lists.
		if (/^[-*+]\s+/.test(t) || /^\d+\.\s+/.test(t)) {
			const ordered = /^\d+\.\s+/.test(t);
			const items = [];
			while (i < lines.length) {
				const m = lines[i].trim().match(/^(?:[-*+]|\d+\.)\s+(.*)$/);
				if (!m) break;
				items.push('<li>' + inline(m[1]) + '</li>');
				i++;
			}
			out.push('<' + (ordered ? 'ol' : 'ul') + '>' + items.join('') + '</' + (ordered ? 'ol' : 'ul') + '>');
			continue;
		}

		// Paragraph: consecutive ordinary lines, folded into one block.
		const buf = [];
		while (i < lines.length) {
			const l = lines[i].trim();
			if (
				l === '' ||
				/^```/.test(l) ||
				/^(#{1,6})\s+/.test(l) ||
				/^[-*+]\s+/.test(l) ||
				/^\d+\.\s+/.test(l) ||
				/^&gt;/.test(l) ||
				/^\|/.test(l)
			) {
				break;
			}
			buf.push(l);
			i++;
		}
		if (buf.length) {
			out.push('<p>' + inline(buf.join(' ')) + '</p>');
			continue;
		}
		// Defensive: a line none of the above matched is emitted as its own
		// paragraph, NEVER dropped. The source is already escaped, so this is
		// safe, and dropping it would be the silent loss this renderer exists to
		// avoid — a stray pipe row showing as text beats the same row vanishing.
		out.push('<p>' + inline(t) + '</p>');
		i++;
	}

	return out.join('\n');
}
