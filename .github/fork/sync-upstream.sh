#!/usr/bin/env bash
# Merge an upstream Xray-core ref into the current branch and verify the result.
# Used by .github/workflows/fork-sync-upstream.yml and by humans/agents locally.
# Never pushes. See FORK_MAINTENANCE.md.
#
# Usage: bash .github/fork/sync-upstream.sh [latest-release|main|<tag>|<sha>]
#
# Exit codes:
#    0  merged and verified; the merge commit is local, not pushed
#   10  already up to date, nothing done
#   20  merge conflict (CI: merge aborted; locally: left in progress to resolve)
#   30  merged cleanly but build/vet/tests failed (semantic conflict)
#    1  usage or environment error

set -uo pipefail

UPSTREAM_URL="https://github.com/XTLS/Xray-core.git"
UPSTREAM_API="https://api.github.com/repos/XTLS/Xray-core/releases/latest"
REF="${1:-latest-release}"

# Packages the fork changes or depends on. Keep in sync with fork-release.yml.
FORK_PACKAGES=(./app/speedlimit/... ./app/dispatcher/... ./common/protocol/... ./infra/conf/... ./core/...)

fail() {
	echo "error: $*" >&2
	exit 1
}

if ! [[ "$REF" =~ ^[A-Za-z0-9][A-Za-z0-9._/-]*$ ]]; then
	fail "invalid upstream ref: $REF"
fi
if ! git diff-index --quiet HEAD --; then
	fail "working tree has uncommitted changes"
fi

if ! git remote get-url upstream >/dev/null 2>&1; then
	git remote add upstream "$UPSTREAM_URL" || fail "cannot add upstream remote"
fi
UPSTREAM_ACTUAL="$(git remote get-url upstream)"
UPSTREAM_ACTUAL="${UPSTREAM_ACTUAL%.git}"
if [[ "${UPSTREAM_ACTUAL,,}" != "${UPSTREAM_URL%.git}" && "${UPSTREAM_ACTUAL,,}" != "https://github.com/xtls/xray-core" ]]; then
	fail "remote 'upstream' points at $UPSTREAM_ACTUAL, expected $UPSTREAM_URL"
fi

if [[ "$REF" == "latest-release" ]]; then
	AUTH=()
	if [[ -n "${GH_TOKEN:-}" ]]; then
		AUTH=(-H "Authorization: Bearer $GH_TOKEN")
	fi
	REF="$(curl -fsSL --retry 3 "${AUTH[@]}" "$UPSTREAM_API" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1)"
	[[ -n "$REF" ]] || fail "cannot resolve the latest upstream release"
	echo "Latest upstream release: $REF"
fi

# FETCH_HEAD rather than tags, so upstream tags never mix with fork release tags.
git fetch --quiet --no-tags upstream "$REF" || fail "cannot fetch upstream ref $REF"
TARGET="$(git rev-parse 'FETCH_HEAD^{commit}')" || fail "cannot resolve FETCH_HEAD"

echo "SYNC_REF=$REF"
echo "SYNC_TARGET=$TARGET"

if git merge-base --is-ancestor "$TARGET" HEAD; then
	echo "Already up to date with upstream $REF."
	exit 10
fi

if ! git merge --no-ff --no-edit -m "Merge upstream Xray-core $REF into fork" "$TARGET"; then
	echo
	echo "MERGE CONFLICT merging upstream $REF. Conflicted files:"
	git diff --name-only --diff-filter=U | sed 's/^/  /'
	if [[ "${CI:-}" == "true" ]]; then
		git merge --abort
	else
		echo "The merge is left in progress. Follow FORK_MAINTENANCE.md, section 'Resolving conflicts'."
	fi
	exit 20
fi

# Files both sides changed. They merged cleanly but deserve a human or agent look.
BASE="$(git merge-base 'HEAD^1' 'HEAD^2')"
OVERLAP="$(comm -12 \
	<(git diff --name-only "$BASE" 'HEAD^2' | sort) \
	<(git diff --name-only "$BASE" 'HEAD^1' | sort))"
if [[ -n "$OVERLAP" ]]; then
	echo
	echo "Changed by both upstream and the fork (review these even though they merged):"
	echo "$OVERLAP" | sed 's/^/  /'
fi

# The fork has its own release matrix; upstream changes here must be ported by hand.
if ! git diff --quiet "$BASE" 'HEAD^2' -- .github/workflows/release.yml .github/build/friendly-filenames.json; then
	echo
	echo "WARNING: upstream changed its release matrix. Port the change to .github/workflows/fork-release.yml."
fi

# Git only checks text. Compile and test to catch semantic conflicts.
echo
echo "Verifying the merge..."
if ! go build ./... || ! go vet "${FORK_PACKAGES[@]}" || ! go test -count=1 -timeout 20m "${FORK_PACKAGES[@]}"; then
	echo
	echo "VERIFY FAILED: upstream $REF merged cleanly but does not build or pass tests."
	echo "See FORK_MAINTENANCE.md, section 'Semantic conflicts'."
	exit 30
fi

echo
echo "Merged and verified upstream $REF: $(git rev-parse HEAD)"
exit 0
