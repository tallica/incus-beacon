#!/bin/sh
# Rewrite a markdown file's repo-relative links to absolute ones at a tag.
#
# Usage: scripts/absolute-links.sh v1.2.3 < README.md
#
# Away from a checkout - on a release page or in a release tarball -
# relative links have nothing to resolve against. Pinning them to the tag
# points each one at the tree that release shipped with. Code blocks and
# code spans are left alone: a link there is an example, not a link.
set -eu

tag=${1:-}
if [ -z "$tag" ]; then
	echo "usage: $0 <tag>" >&2
	exit 2
fi

awk -v blob="https://github.com/tallica/incus-beacon/blob/$tag/" \
	-v raw="https://raw.githubusercontent.com/tallica/incus-beacon/$tag/" '
# Apply rewrite() to every match of re in s.
function each(s, re, image,    out) {
	out = ""
	while (match(s, re)) {
		out = out substr(s, 1, RSTART - 1) rewrite(substr(s, RSTART, RLENGTH), image)
		s = substr(s, RSTART + RLENGTH)
	}
	return out s
}
# An embedded image wants the file itself, not the page GitHub wraps it in.
function rewrite(m, image,    i) {
	i = index(m, "](") + 1
	return substr(m, 1, i) (image ? raw : blob) substr(m, i + 1)
}
function links(s) {
	s = each(s, "!\\[[^]]*\\]\\([A-Za-z0-9._/-]+\\)", 1)
	return each(s, "\\]\\([A-Za-z0-9._/-][A-Za-z0-9._/#-]*\\)", 0)
}
# A code span opens with a run of backticks and closes at the next run of
# the same length; an unmatched run is literal text.
function prose(s,    out, run, len, rest, n, j) {
	out = ""
	while (match(s, /`+/)) {
		run = substr(s, RSTART, RLENGTH)
		len = RLENGTH
		rest = substr(s, RSTART + len)
		out = out links(substr(s, 1, RSTART - 1))
		n = 0
		for (j = 1; j <= length(rest); j++) {
			if (substr(rest, j, len) == run && substr(rest, j + len, 1) != "`" &&
			    (j == 1 || substr(rest, j - 1, 1) != "`")) { n = j; break }
		}
		if (n) {
			out = out run substr(rest, 1, n - 1 + len)
			s = substr(rest, n + len)
		} else {
			out = out run
			s = rest
		}
	}
	return out links(s)
}
# Fences: three or more backticks or tildes, closed by at least as many.
fence == "" && match($0, /^ ? ? ?(```+|~~~+)/) {
	fence = substr($0, RSTART, RLENGTH); sub(/^ +/, "", fence)
	print; next
}
fence != "" {
	line = $0; sub(/^ +/, "", line)
	if (index(line, fence) == 1 && line ~ /^(`+|~+)[ \t]*$/) fence = ""
	print; next
}
{ print prose($0) }
'
