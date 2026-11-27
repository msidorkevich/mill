#!/bin/sh
# Example mill hook (on_publish / on_update): opens or updates the Bitbucket Cloud pull request
# for a task's review branch. POSIX sh; needs only git, curl and jq.
#
# Input: the mill/hook@1 JSON document on stdin.
# Environment:
#   BITBUCKET_TOKEN     API or repository access token with pull request read/write, sent as a
#                       Bearer token (list it in hook_env)
#   BITBUCKET_USER      when set, BITBUCKET_USER:BITBUCKET_TOKEN is sent as basic auth instead
#                       (Atlassian account email or username with an API token / app password)
#   MILL_BITBUCKET_API  API base URL (default https://api.bitbucket.org/2.0; tests)
#   MILL_HOOK_CHECK     when 1, check the prerequisites and exit (mill doctor)
#
# On success it prints the PR's links.html.href.

set -eu

api=${MILL_BITBUCKET_API:-https://api.bitbucket.org/2.0}
api=${api%/}

die() {
	echo "bitbucket hook: $*" >&2
	exit 1
}

if [ "${MILL_HOOK_CHECK:-}" = 1 ]; then
	command -v curl >/dev/null 2>&1 || die "curl not found: install curl"
	command -v jq >/dev/null 2>&1 || die "jq not found: install jq"
fi

tmp=$(mktemp -d "${TMPDIR:-/tmp}/mill-bb-hook.XXXXXX") || die "cannot create a temporary directory"
trap 'rm -rf "$tmp"' EXIT

# repo_from_url URL: prints workspace/repo for a Bitbucket SSH or HTTPS remote URL.
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
# status and the response message.
api_call() {
	method=$1
	url="$api$2"
	out="$tmp/response"
	has_body=0
	if [ $# -ge 3 ]; then
		printf '%s' "$3" >"$tmp/request"
		has_body=1
	fi
	set -- -sS -o "$out" -w '%{http_code}' -X "$method" -H "Accept: application/json"
	if [ -n "${BITBUCKET_USER:-}" ]; then
		set -- "$@" -u "$BITBUCKET_USER:$BITBUCKET_TOKEN"
	else
		set -- "$@" -H "Authorization: Bearer $BITBUCKET_TOKEN"
	fi
	if [ "$has_body" = 1 ]; then
		set -- "$@" -H "Content-Type: application/json" --data-binary "@$tmp/request"
	fi
	status=$(curl "$@" "$url") || die "$method $url failed: curl could not reach the API (check MILL_BITBUCKET_API and the network)"
	case $status in
	2??) cat "$out" ;;
	*)
		msg=$(jq -r '.error.message // .message // empty' "$out" 2>/dev/null) || msg=""
		[ -n "$msg" ] || msg=$(cat "$out")
		die "$method $url failed: HTTP $status: $msg"
		;;
	esac
}

urlencode() {
	jq -rn --arg v "$1" '$v | @uri'
}

# find_pr BRANCH: prints the id of the open PR from BRANCH, or nothing.
find_pr() {
	query=$(jq -rn --arg b "$1" '"source.branch.name=" + ($b | tojson) + " AND state=\"OPEN\""')
	q=$(urlencode "$query")
	list=$(api_call GET "/repositories/$repo/pullrequests?q=$q")
	printf '%s' "$list" | jq -r '(.values // []) | if length > 0 then .[0].id else empty end' ||
		die "unexpected response listing pull requests for $1"
}

need_token() {
	[ -n "${BITBUCKET_TOKEN:-}" ] ||
		die "set BITBUCKET_TOKEN (and BITBUCKET_USER for basic auth) and list them in hook_env"
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
	repo=$(repo_from_url "$remote_url") || die "cannot parse workspace/repo from remote URL $remote_url"
	api_call GET "/repositories/$repo" >/dev/null
	echo "bitbucket hook: ok ($repo)"
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
repo=$(repo_from_url "$remote_url") || die "cannot parse workspace/repo from remote URL $remote_url"

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

id=$(find_pr "$branch")
if [ -n "$id" ]; then
	payload=$(jq -n --arg base "$base" --arg title "$title" --arg body "$body" \
		'{title: $title, description: $body, destination: {branch: {name: $base}}}')
	resp=$(api_call PUT "/repositories/$repo/pullrequests/$id" "$payload")
else
	payload=$(jq -n --arg head "$branch" --arg base "$base" --arg title "$title" --arg body "$body" \
		'{title: $title, description: $body, source: {branch: {name: $head}}, destination: {branch: {name: $base}}, draft: true}')
	resp=$(api_call POST "/repositories/$repo/pullrequests" "$payload")
fi
printf '%s' "$resp" | jq -r '.links.html.href // empty'
