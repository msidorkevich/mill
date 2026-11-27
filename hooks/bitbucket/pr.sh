#!/bin/sh
# Example mill hook (on_publish / on_update / status): opens or updates the Bitbucket Cloud pull
# request for a task's review branch, or reports the build statuses of its head commit. POSIX sh;
# needs only git, curl and jq.
#
# Input: the mill/hook@1 JSON document on stdin.
# Environment:
#   BITBUCKET_TOKEN     API or repository access token with pull request read/write (and for
#                       status repository and pipelines read), sent as a Bearer token (list it
#                       in hook_env)
#   BITBUCKET_USER      when set, BITBUCKET_USER:BITBUCKET_TOKEN is sent as basic auth instead
#                       (Atlassian account email or username with an API token / app password)
#   MILL_BITBUCKET_API  API base URL (default https://api.bitbucket.org/2.0; tests)
#   MILL_BITBUCKET_DRAFT when 0, new PRs are created ready for review instead of as drafts
#   MILL_HOOK_EVENT     the event (publish, update, status); overrides the document's event
#   MILL_HOOK_CHECK     when 1, check the prerequisites and exit (mill doctor)
#
# On publish/update it prints the PR's links.html.href. On status it prints one mill/status@1
# object:
#   {"schema": "mill/status@1", "state": "success|failure|pending|none", "url": PR html link,
#    "checks": [{"name", "url", "state", "summary" (failures only)}]}

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

# config_quote VALUE: prints VALUE with \ and " escaped for a double-quoted curl config value.
# printf is a shell builtin, so VALUE never appears in another process's arguments.
config_quote() {
	printf '%s' "$1" | sed 's/[\\"]/\\&/g'
}

# auth_config: prints the curl config line carrying the credentials.
auth_config() {
	token=$(config_quote "$BITBUCKET_TOKEN") || return 1
	if [ -n "${BITBUCKET_USER:-}" ]; then
		user=$(config_quote "$BITBUCKET_USER") || return 1
		printf 'user = "%s:%s"\n' "$user" "$token"
	else
		printf 'header = "Authorization: Bearer %s"\n' "$token"
	fi
}

# api_call METHOD PATH [JSON-BODY]: prints the response body; fails on any HTTP error with the
# status and the response message.
api_call() {
	m=$1
	u="$api$2"
	shift 2
	api_call_url "$m" "$u" "$@"
}

# api_call_url METHOD URL [JSON-BODY]: api_call for an absolute URL (a page's next link).
api_call_url() {
	api_request "$@"
	case $status in
	2??) cat "$out" ;;
	*)
		msg=$(jq -r '.error.message // .message // empty' "$out" 2>/dev/null) || msg=""
		[ -n "$msg" ] || msg=$(cat "$out")
		die "$method $url failed: HTTP $status: $msg"
		;;
	esac
}

# api_request METHOD URL [JSON-BODY]: sends the request and sets status to the HTTP status code,
# leaving the response body in $out; fails only when curl cannot reach the API.
api_request() {
	method=$1
	url=$2
	out="$tmp/response"
	has_body=0
	if [ $# -ge 3 ]; then
		printf '%s' "$3" >"$tmp/request"
		has_body=1
	fi
	set -- -sS -o "$out" -w '%{http_code}' -X "$method" -H "Accept: application/json"
	if [ "$has_body" = 1 ]; then
		set -- "$@" -H "Content-Type: application/json" --data-binary "@$tmp/request"
	fi
	# The credentials go to curl as a config on stdin, never on its command line (visible in ps).
	auth=$(auth_config) || die "cannot build the curl auth config"
	status=$(curl -K - "$@" "$url" <<EOF
$auth
EOF
) || die "$method $url failed: curl could not reach the API (check MILL_BITBUCKET_API and the network)"
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

# find_pr_url BRANCH: prints the html link of the open PR from BRANCH, or nothing.
find_pr_url() {
	query=$(jq -rn --arg b "$1" '"source.branch.name=" + ($b | tojson) + " AND state=\"OPEN\""')
	q=$(urlencode "$query")
	list=$(api_call GET "/repositories/$repo/pullrequests?q=$q")
	printf '%s' "$list" | jq -r '(.values // []) | if length > 0 then .[0].links.html.href // empty else empty end' ||
		die "unexpected response listing pull requests for $1"
}

# all_values PATH: prints one JSON array of the values of every page, following each next link.
all_values() {
	page_url="$api$1"
	pages="$tmp/pages"
	: >"$pages"
	while [ -n "$page_url" ]; do
		case $page_url in
		"$api"/*) ;;
		*) die "the next page link $page_url is not under $api: refusing to send the token there" ;;
		esac
		page=$(api_call_url GET "$page_url")
		printf '%s' "$page" | jq -c '.values // []' >>"$pages" ||
			die "unexpected response from GET $page_url"
		page_url=$(printf '%s' "$page" | jq -r '.next // empty') ||
			die "unexpected response from GET $page_url"
	done
	jq -s 'add // []' "$pages" || die "cannot combine the pages of $1"
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
	info=$(api_call GET "/repositories/$repo")

	# An empty pull request: Bitbucket answers 400 (missing fields) only when the token may
	# create pull requests, so nothing is ever created.
	api_request POST "$api/repositories/$repo/pullrequests" '{}'
	case $status in
	400) ;;
	401 | 403) die "the token cannot create pull requests: give it Pull requests: Write on $repo" ;;
	*) die "POST /repositories/$repo/pullrequests (permission probe) answered HTTP $status, want 400: check the token and MILL_BITBUCKET_API" ;;
	esac

	if [ "${MILL_HOOK_EVENT:-}" = status ]; then
		default=$(printf '%s' "$info" | jq -r '.mainbranch.name // empty') ||
			die "unexpected response reading $repo"
		[ -n "$default" ] ||
			die "GET /repositories/$repo did not name the main branch: check the token's access to $repo"
		branch_path=$(urlencode "$default")
		api_request GET "$api/repositories/$repo/commit/$branch_path/statuses"
		[ "$status" = 200 ] ||
			die "the token cannot read commit statuses (HTTP $status): give it Repositories: Read and Pipelines: Read on $repo"
	fi
	echo "bitbucket hook: ok ($repo; token may create pull requests)"
	exit 0
fi

need_token
input=$(cat)
field() {
	printf '%s' "$input" | jq -r --arg k "$1" '.[$k] // empty' || die "stdin is not valid mill/hook@1 JSON"
}
remote_url=$(field remote_url)
branch=$(field review_branch)
event=${MILL_HOOK_EVENT:-$(field event)}

if [ "$event" = status ]; then
	head_sha=$(field head_sha)
	[ -n "$remote_url" ] || die "remote_url missing from the hook input"
	[ -n "$head_sha" ] || die "head_sha missing from the hook input"
	repo=$(repo_from_url "$remote_url") || die "cannot parse workspace/repo from remote URL $remote_url"
	sha_path=$(urlencode "$head_sha")
	statuses=$(all_values "/repositories/$repo/commit/$sha_path/statuses")
	pr_url=""
	if [ -n "$branch" ]; then
		pr_url=$(find_pr_url "$branch")
	fi
	jq -n --argjson statuses "$statuses" --arg url "$pr_url" '
		# cut: s cut to at most n bytes of UTF-8, on a character boundary.
		def cut($n):
			reduce explode[] as $c ({len: 0, out: [], full: false};
				if .full then .
				else ($c | if . < 128 then 1 elif . < 2048 then 2 elif . < 65536 then 3 else 4 end) as $w
					| if .len + $w > $n then .full = true else .len += $w | .out += [$c] end
				end)
			| .out | implode;
		# An unknown state is pending: it never counts as success.
		def status_state:
			if . == "FAILED" or . == "STOPPED" then "failure"
			elif . == "SUCCESSFUL" then "success"
			else "pending" end;
		[$statuses[]
			| (.state | status_state) as $s
			| {name: (.name // .key // ""), url: (.url // ""), state: $s}
			+ (if $s == "failure" then {summary: ((.description // "") | cut(2048))} else {} end)
		] as $checks
		| {
			schema: "mill/status@1",
			state: (if any($checks[]; .state == "failure") then "failure"
				elif any($checks[]; .state == "pending") then "pending"
				elif ($checks | length) > 0 then "success"
				else "none" end),
			checks: $checks
		}
		+ (if $url != "" then {url: $url} else {} end)' ||
		die "unexpected response reading the statuses of $head_sha"
	exit 0
fi

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

draft=true
if [ "${MILL_BITBUCKET_DRAFT:-}" = 0 ]; then
	draft=false
fi

id=$(find_pr "$branch")
if [ -n "$id" ]; then
	payload=$(jq -n --arg base "$base" --arg title "$title" --arg body "$body" \
		'{title: $title, description: $body, destination: {branch: {name: $base}}}')
	resp=$(api_call PUT "/repositories/$repo/pullrequests/$id" "$payload")
else
	payload=$(jq -n --arg head "$branch" --arg base "$base" --arg title "$title" --arg body "$body" \
		--argjson draft "$draft" \
		'{title: $title, description: $body, source: {branch: {name: $head}}, destination: {branch: {name: $base}}, draft: $draft}')
	resp=$(api_call POST "/repositories/$repo/pullrequests" "$payload")
fi
printf '%s' "$resp" | jq -r '.links.html.href // empty'
