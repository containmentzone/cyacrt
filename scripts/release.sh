#!/bin/sh
# Usage: scripts/release.sh proud|default|shame|<x.y.z>
#
# Bumps VERSION following PrideVer (https://pridever.org),
# PROUD.DEFAULT.SHAME, and regenerates CHANGELOG.md from the pull requests
# merged into main (first-parent history) between v* tags. Bumping one part
# zeroes the parts after it. It commits and tags nothing: merge the result,
# then tag that merge with v<x.y.z>. A release pull request titled
# "Release v<x.y.z>" is left out of the changelog.
set -eu
cd "$(dirname "$0")/.."
repo=https://github.com/containmentzone/cyacrt

IFS=. read -r proud default shame <VERSION
case "${1-}" in
proud) next=$((proud + 1)).0.0 ;;
default) next=$proud.$((default + 1)).0 ;;
shame) next=$proud.$default.$((shame + 1)) ;;
[0-9]*.[0-9]*.[0-9]*) next=$1 ;;
*)
	echo "usage: $0 proud|default|shame|<x.y.z>" >&2
	exit 2
	;;
esac
if git rev-parse -q --verify "refs/tags/v$next" >/dev/null; then
	echo "v$next is already tagged" >&2
	exit 1
fi

# md escapes what Markdown would take for HTML, like <name>.
md() { printf '%s\n' "$1" | sed 's/</\\</g'; }

# section <heading> <date> <rev-list range>
section() {
	printf '\n## %s (%s)\n\n' "$1" "$2"
	git rev-list --first-parent "$3" | while read -r c; do
		s=$(git log -1 --format=%s "$c")
		case $s in
		"Merge pull request #"*)
			n=${s#Merge pull request #}
			n=${n%% *}
			t=$(git log -1 --format=%b "$c" | head -n1)
			case $t in "Release v"*) continue ;; esac
			echo "- $(md "$t") ([#$n]($repo/pull/$n))"
			;;
		*) echo "- $(md "$s")" ;;
		esac
	done
}

{
	echo "# Changelog"
	echo
	echo "Versions follow [PrideVer](https://pridever.org): PROUD.DEFAULT.SHAME."
	echo "scripts/release.sh generates this file from merged pull requests; don't edit it by hand."
	name=v$next date=$(date -u +%F) end=HEAD
	for t in $(git tag -l 'v*' --sort=-v:refname); do
		section "$name" "$date" "$t..$end"
		name=$t date=$(git log -1 --format=%cs "$t") end=$t
	done
	section "$name" "$date" "$end"
} >CHANGELOG.md
echo "$next" >VERSION

echo "VERSION and CHANGELOG.md are at v$next. Open a pull request titled"
echo "\"Release v$next\"; once it is merged: git tag v$next <merge commit> && git push origin v$next"
