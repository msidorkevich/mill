#!/bin/sh
# Example mill hook (on_publish / on_update): opens or updates the GitHub pull request for a
# task's review branch. POSIX sh; needs only git, curl and jq.
#
# Input: the mill/hook@1 JSON document on stdin.
# Environment:
#   GITHUB_TOKEN     token with pull requests read/write (list it in hook_env)
#   MILL_GITHUB_API  API base URL (default https://api.github.com; GitHub Enterprise, tests)
#   MILL_HOOK_CHECK  when 1, check the prerequisites and exit (mill doctor)
#
# On success it prints the PR's html_url.

set -eu

api=${MILL_GITHUB_API:-https://api.github.com}
api=${api%/}

die() {
	echo "github hook: $*" >&2
	exit 1
}

if [ "${MILL_HOOK_CHECK:-}" = 1 ]; then
	command -v curl >/dev/null 2>&1 || die "curl not found: install curl"
	command -v jq >/dev/null 2>&1 || die "jq not found: install jq"
fi

tmp=$(mktemp -d "${TMPDIR:-/tmp}/mill-gh-hook.XXXXXX") || die "cannot create a temporary directory"
trap 'rm -rf "$tmp"' EXIT

# repo_from_url URL: prints owner/repo for a GitHub SSH or HTTPS remote URL.
repo_from_url() {
	path=$1
	case $path in
	git@*:*) path=${path#*:} ;;
	ssh://*) path=${path#ssh://}; path=${path#*/} ;;
	https://* | http://*) path=${path#*://}; path=${path#*/} ;;
	*) return 1 ;;
	esac
	path=${path%/}
	path=${path%.git}
	case $path in
	*/*/* | /* | */ | "") return 1 ;;
	*/*) printf '%s\n' "$path" ;;
	*) return 1 ;;
	esac
}

# api_call METHOD PATH [JSON-BODY]: prints the response body; fails on any HTTP error with the
# status and the response message. The token reaches curl as a config on stdin (-K -), never on
# its command line, so other users cannot read it from the process list; the request body goes
# through a file because stdin carries that config.
api_call() {
	method=$1
	url="$api$2"
	out="$tmp/response"
	if [ $# -ge 3 ]; then
		printf '%s' "$3" >"$tmp/request"
		status=$(curl -K - -sS -o "$out" -w '%{http_code}' -X "$method" \
			-H "Accept: application/vnd.github+json" \
			-H "X-GitHub-Api-Version: 2022-11-28" \
			-H "Content-Type: application/json" \
			--data-binary "@$tmp/request" "$url" <<EOF
header = "Authorization: Bearer $GITHUB_TOKEN"
EOF
		) || die "$method $url failed: curl could not reach the API (check MILL_GITHUB_API and the network)"
	else
		status=$(curl -K - -sS -o "$out" -w '%{http_code}' -X "$method" \
			-H "Accept: application/vnd.github+json" \
			-H "X-GitHub-Api-Version: 2022-11-28" \
			"$url" <<EOF
header = "Authorization: Bearer $GITHUB_TOKEN"
EOF
		) || die "$method $url failed: curl could not reach the API (check MILL_GITHUB_API and the network)"
	fi
	case $status in
	2??) cat "$out" ;;
	*)
		msg=$(jq -r '.message // empty' "$out" 2>/dev/null) || msg=""
		[ -n "$msg" ] || msg=$(cat "$out")
		die "$method $url failed: HTTP $status: $msg"
		;;
	esac
}

urlencode() {
	jq -rn --arg v "$1" '$v | @uri'
}

# find_pr BRANCH: prints the number of the open PR from BRANCH, or nothing.
find_pr() {
	head=$(urlencode "$owner:$1")
	list=$(api_call GET "/repos/$repo/pulls?head=$head&state=open")
	printf '%s' "$list" | jq -r 'if type == "array" and length > 0 then .[0].number else empty end' ||
		die "unexpected response listing pull requests for $1"
}

need_token() {
	[ -n "${GITHUB_TOKEN:-}" ] || die "set GITHUB_TOKEN and list it in hook_env"
}

if [ "${MILL_HOOK_CHECK:-}" = 1 ]; then
	need_token
	input=""
	if [ ! -t 0 ]; then
		input=$(cat)
	fi
	remote_url=""
	if [ -n "$input" ]; then
		remote_url=$(printf '%s' "$input" | jq -r '.remote_url // empty') ||
			die "stdin is not valid JSON"
	fi
	if [ -z "$remote_url" ]; then
		remote_url=$(git remote get-url "${MILL_REMOTE:-origin}" 2>/dev/null) ||
			die "cannot read the remote URL: run inside the repository or set MILL_REMOTE"
	fi
	repo=$(repo_from_url "$remote_url") || die "cannot parse owner/repo from remote URL $remote_url"
	api_call GET "/repos/$repo" >/dev/null
	echo "github hook: ok ($repo)"
	exit 0
fi

need_token
input=$(cat)
field() {
	printf '%s' "$input" | jq -r --arg k "$1" '.[$k] // empty' || die "stdin is not valid mill/hook@1 JSON"
}
remote_url=$(field remote_url)
branch=$(field review_branch)
base=$(field base_branch)
title=$(field title)
description=$(field description)
parent_branch=$(field parent_review_branch)

[ -n "$remote_url" ] || die "remote_url missing from the hook input"
[ -n "$branch" ] || die "review_branch missing from the hook input"
[ -n "$base" ] || die "base_branch missing from the hook input"
repo=$(repo_from_url "$remote_url") || die "cannot parse owner/repo from remote URL $remote_url"
owner=${repo%%/*}

body=$description
if [ -n "$parent_branch" ]; then
	parent=$(find_pr "$parent_branch")
	if [ -n "$parent" ]; then
		stacked="Stacked on #$parent: merge that first"
	else
		stacked="Stacked on $parent_branch"
	fi
	body=$(printf '%s\n\n%s' "$stacked" "$description")
fi

number=$(find_pr "$branch")
if [ -n "$number" ]; then
	payload=$(jq -n --arg base "$base" --arg title "$title" --arg body "$body" \
		'{base: $base, title: $title, body: $body}')
	resp=$(api_call PATCH "/repos/$repo/pulls/$number" "$payload")
else
	payload=$(jq -n --arg head "$branch" --arg base "$base" --arg title "$title" --arg body "$body" \
		'{head: $head, base: $base, title: $title, body: $body, draft: true}')
	resp=$(api_call POST "/repos/$repo/pulls" "$payload")
fi
printf '%s' "$resp" | jq -r '.html_url // empty'
