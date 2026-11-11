#!/bin/sh
# Write THIRD_PARTY_NOTICES.md from the modules the release binaries link:
# each one's licence text and any NOTICE file beside it. With --check, compare
# instead and fail if the committed file is stale. Both fail first on a module
# whose licence is not in the allowed list below, or that has none.
#
# Usage: scripts/notices.sh [--check]
set -eu

go_licenses=github.com/google/go-licenses/v2@v2.0.1
allowed=MIT,BSD-2-Clause,BSD-3-Clause,Apache-2.0,ISC,Unlicense
# Modules with no licence file that may be linked anyway, as module=licence,
# comma-separated; a comment above says why each one is fine.
# go-proto: its repo, github.com/rootless-containers/proto, is Apache-2.0
# (COPYING at the root), as its files' headers say; the module ships no copy.
unlicensed=github.com/rootless-containers/proto/go-proto=Apache-2.0
# Modules whose licence go-licenses misreads, as module=licence=file, the file
# relative to the module; a comment above says why each one is right.
# filepath-securejoin, via the Incus client: go-licenses reads COPYING.md as
# MPL-2.0, but it covers BSD-3-Clause and MPL-2.0 files alike, and the files
# linked (doc.go, join.go, vfs.go, internal/consts) are BSD-3-Clause by their
# SPDX headers. Re-check those when it's bumped.
overrides=github.com/cyphar/filepath-securejoin=BSD-3-Clause=LICENSE.BSD
out=THIRD_PARTY_NOTICES.md

case "$#:${1-}" in
0:) check=0 ;;
1:--check) check=1 ;;
*)
	echo "usage: $0 [--check]" >&2
	exit 2
	;;
esac

# Build constraints can link other modules on another OS, so list them for
# every OS GoReleaser builds for, with CGO off as in the release builds.
goos=$(sed -n '/^    goos:/,/^    [a-z]/s/^      - //p' .goreleaser.yaml)
[ -n "$goos" ] || {
	echo "$0: no goos list in .goreleaser.yaml" >&2
	exit 1
}

tmp=$(mktemp -d "${TMPDIR:-/tmp}/notices.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
# For this machine, whatever GOOS the caller set.
GOBIN=$tmp GOOS='' GOARCH='' go install "$go_licenses"
module=$(go list -m)
printf '%s\n' '{{range .}}{{.LicensePath}}	{{.LicenseName}}	{{.Name}}' '{{end}}' >"$tmp/report.tmpl"
for os in $goos; do
	CGO_ENABLED=0 GOOS=$os "$tmp/go-licenses" report --template "$tmp/report.tmpl" \
		--ignore "$module" ./cmd/... >>"$tmp/libs" 2>"$tmp/log" || {
		cat "$tmp/log" >&2
		exit 1
	}
done
# Apply overrides=, and give each unlicensed= module its licence and a path
# of its own, so that two of them don't share a section.
sort -u "$tmp/libs" | grep . | awk -F '\t' -v OFS='\t' -v overrides="$overrides" -v unlicensed="$unlicensed" '
	BEGIN {
		n = split(overrides, list, ",")
		for (i = 1; i <= n; i++)
			if (split(list[i], f, "=") == 3) { lic[f[1]] = f[2]; file[f[1]] = f[3] }
		n = split(unlicensed, list, ",")
		for (i = 1; i <= n; i++)
			if (split(list[i], f, "=") == 2) none[f[1]] = f[2]
	}
	$3 in lic { sub(/\/[^\/]*$/, "/" file[$3], $1); $2 = lic[$3] }
	$2 == "Unknown" && $3 in none { $1 = "unlicensed:" $3; $2 = none[$3] }
	{ print }' >"$tmp/sorted" || true

bad=$(awk -F '\t' -v allowed=",$allowed," '
	index(allowed, "," $2 ",") == 0 { print "  " $3 ": " $2; next }
	$1 ~ /^unlicensed:/ || $1 in seen { next }
	{ seen[$1] = 1 }
	(getline line < $1) <= 0 { print "  " $3 ": no file at " $1 }
	{ close($1) }' "$tmp/sorted")
[ -z "$bad" ] || {
	printf '%s\n' "$0: licences not allowed or not found (see allowed=, unlicensed= and overrides= in this script):" "$bad" >&2
	exit 1
}

{
	cat <<'EOF'
# Third-party notices

This code is MIT-licensed (see [LICENSE](LICENSE)). Release binaries statically
link the Go modules below, whose licences require these notices to travel with
the binaries. `make notices` writes this file from the modules the binaries
link; do not edit it by hand.

## Go standard library (BSD-3-Clause)

The Go runtime and standard library, from https://go.dev/LICENSE:

```text
Copyright 2009 The Go Authors.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are
met:

   * Redistributions of source code must retain the above copyright
notice, this list of conditions and the following disclaimer.
   * Redistributions in binary form must reproduce the above
copyright notice, this list of conditions and the following disclaimer
in the documentation and/or other materials provided with the
distribution.
   * Neither the name of Google LLC nor the names of its
contributors may be used to endorse or promote products derived from
this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
"AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
(INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
```
EOF
	# One section per licence file. A module in the cache is named by its
	# directory there, without the @version and with the cache's !x escapes
	# for capitals undone, so a dependency bump alone changes nothing.
	awk -F '\t' -v cache="$(go env GOMODCACHE)/" '
		!($1 in names) { order[++n] = $1; names[$1] = $2; pkg[$1] = $3; next }
		index(", " names[$1] ", ", ", " $2 ", ") == 0 { names[$1] = names[$1] ", " $2 }
		END {
			for (i = 1; i <= n; i++) {
				path = order[i]
				name = pkg[path]
				if (index(path, cache) == 1) {
					name = substr(path, length(cache) + 1)
					sub(/\/[^\/]*$/, "", name)
					sub(/@[^\/]*/, "", name)
					while (match(name, /![a-z]/))
						name = substr(name, 1, RSTART - 1) toupper(substr(name, RSTART + 1, 1)) substr(name, RSTART + 2)
				}
				print name "\t" path "\t" names[path]
			}
		}' "$tmp/sorted" | LC_ALL=C sort | awk -F '\t' '
		# Print a file in a code block: CRs and blank edge lines dropped, the
		# fence longer than any run of backticks inside.
		function block(file,    line, k, lines, first, last, run, fence) {
			k = 0
			while ((getline line < file) > 0) {
				sub(/\r$/, "", line)
				lines[++k] = line
				while (match(line, /`+/)) {
					if (RLENGTH > run) run = RLENGTH
					line = substr(line, RSTART + RLENGTH)
				}
			}
			close(file)
			for (first = 1; first <= k && lines[first] ~ /^[ \t]*$/; first++);
			for (last = k; last >= first && lines[last] ~ /^[ \t]*$/; last--);
			fence = "```"
			while (length(fence) <= run) fence = fence "`"
			print fence "text"
			for (; first <= last; first++) print lines[first]
			print fence
		}
		{
			printf "\n## `%s` (%s)\n\n", $1, $3
			if ((getline line < $2) <= 0) {
				print "The module has no licence file; the headers of its files name this licence."
				next
			}
			close($2)
			printf "From <https://pkg.go.dev/%s>:\n\n", $1
			block($2)
			dir = $2
			sub(/\/[^\/]*$/, "", dir)
			split("NOTICE NOTICE.txt NOTICE.md", notices, " ")
			for (i = 1; i <= 3; i++)
				if (dir "/" notices[i] != $2 && (getline line < (dir "/" notices[i])) > 0) {
					close(dir "/" notices[i])
					printf "\nIts %s file:\n\n", notices[i]
					block(dir "/" notices[i])
				}
		}'
} >"$tmp/$out"

if [ "$check" -eq 1 ]; then
	diff -u "$out" "$tmp/$out" >&2 || {
		echo "$0: $out is stale: run make notices" >&2
		exit 1
	}
else
	cp "$tmp/$out" "$out"
fi
