#!/bin/sh
# Example mill hook (on_publish / on_update / status): opens or updates the GitHub pull request
# for a task's review branch, or reports the CI state of its head commit. POSIX sh; needs only
# git, curl and jq.
#
# Input: the mill/hook@1 JSON document on stdin.
# Environment:
#   GITHUB_TOKEN      token with pull requests read/write, and for status commit statuses and
#                     checks read (list it in hook_env)
#   MILL_GITHUB_API   API base URL (default https://api.github.com; GitHub Enterprise, tests)
#   MILL_GITHUB_DRAFT when 0, new PRs are created ready for review instead of as drafts
#   MILL_HOOK_EVENT   the event (publish, update, status); overrides the document's event
#   MILL_HOOK_CHECK   when 1, check the prerequisites and exit (mill doctor)
#
# On publish/update it prints the PR's html_url. On status it prints one mill/status@1 object:
#   {"schema": "mill/status@1", "state": "success|failure|pending|none", "url": PR html_url,
#    "checks": [{"name", "url", "state", "summary" (failures only)}]}

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
	api_request "$@"
	case $status in
	2??) cat "$out" ;;
	*)
		msg=$(jq -r '.message // empty' "$out" 2>/dev/null) || msg=""
		[ -n "$msg" ] || msg=$(cat "$out")
		die "$method $url failed: HTTP $status: $msg"
		;;
	esac
}

# api_request METHOD PATH [JSON-BODY]: sends the request and sets status to the HTTP status code,
# leaving the response body in $out; fails only when curl cannot reach the API.
api_request() {
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

# find_pr_url BRANCH: prints the html_url of the open PR from BRANCH, or nothing.
find_pr_url() {
	head=$(urlencode "$owner:$1")
	list=$(api_call GET "/repos/$repo/pulls?head=$head&state=open")
	printf '%s' "$list" | jq -r 'if type == "array" and length > 0 then .[0].html_url // empty else empty end' ||
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
	info=$(api_call GET "/repos/$repo")
	default=$(printf '%s' "$info" | jq -r '.default_branch // empty') ||
		die "unexpected response reading $repo"
	[ -n "$default" ] || die "GET /repos/$repo did not name the default branch: check the token's access to $repo"

	# A PR from a branch that does not exist: GitHub answers 422 only when the token may create
	# pull requests, so nothing is ever created.
	probe=mill-permission-probe-$(od -An -N6 -tx1 /dev/urandom | tr -d ' \n') ||
		die "cannot read /dev/urandom for the probe branch name"
	payload=$(jq -n --arg head "$probe" --arg base "$default" \
		'{head: $head, base: $base, title: "mill permission probe"}')
	api_request POST "/repos/$repo/pulls" "$payload"
	case $status in
	422) ;;
	403 | 404) die "the token cannot create pull requests: give it Pull requests: Read and write on $repo" ;;
	*) die "POST /repos/$repo/pulls (permission probe) answered HTTP $status, want 422: check the token and MILL_GITHUB_API" ;;
	esac

	if [ "${MILL_HOOK_EVENT:-}" = status ]; then
		branch_path=$(urlencode "$default")
		api_request GET "/repos/$repo/commits/$branch_path/status"
		[ "$status" = 200 ] ||
			die "the token cannot read commit statuses (HTTP $status): give it Commit statuses: Read and Checks: Read on $repo"
	fi
	echo "github hook: ok ($repo; token may create pull requests)"
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
	repo=$(repo_from_url "$remote_url") || die "cannot parse owner/repo from remote URL $remote_url"
	owner=${repo%%/*}
	statuses=$(api_call GET "/repos/$repo/commits/$head_sha/status?per_page=100")
	runs=$(api_call GET "/repos/$repo/commits/$head_sha/check-runs?per_page=100")
	pr_url=""
	if [ -n "$branch" ]; then
		pr_url=$(find_pr_url "$branch")
	fi
	jq -n --argjson statuses "$statuses" --argjson runs "$runs" --arg url "$pr_url" '
		# cut: s cut to at most n bytes of UTF-8, on a character boundary.
		def cut($n):
			reduce explode[] as $c ({len: 0, out: [], full: false};
				if .full then .
				else ($c | if . < 128 then 1 elif . < 2048 then 2 elif . < 65536 then 3 else 4 end) as $w
					| if .len + $w > $n then .full = true else .len += $w | .out += [$c] end
				end)
			| .out | implode;
		def status_state:
			if . == "failure" or . == "error" then "failure"
			elif . == "pending" then "pending"
			else "success" end;
		def run_state:
			if .status != "completed" then "pending"
			elif (.conclusion | IN("failure", "timed_out", "cancelled", "action_required", "startup_failure")) then "failure"
			else "success" end;
		([($statuses.statuses // [])[] | {name: .context, url: (.target_url // ""), state: (.state | status_state)}]
		 + [($runs.check_runs // [])[]
			| run_state as $s
			| {name: .name, url: (.html_url // ""), state: $s}
			+ (if $s == "failure" then {summary: (((.output.title // "") + "\n" + (.output.summary // "")) | cut(2048))} else {} end)]
		) as $checks
		| {
			schema: "mill/status@1",
			state: (if any($checks[]; .state == "failure") then "failure"
				elif any($checks[]; .state == "pending") then "pending"
				elif ($checks | length) > 0 then "success"
				else "none" end),
			checks: $checks
		}
		+ (if $url != "" then {url: $url} else {} end)' ||
		die "unexpected response reading the statuses and check runs of $head_sha"
	exit 0
fi

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

draft=true
if [ "${MILL_GITHUB_DRAFT:-}" = 0 ]; then
	draft=false
fi

number=$(find_pr "$branch")
if [ -n "$number" ]; then
	payload=$(jq -n --arg base "$base" --arg title "$title" --arg body "$body" \
		'{base: $base, title: $title, body: $body}')
	resp=$(api_call PATCH "/repos/$repo/pulls/$number" "$payload")
else
	payload=$(jq -n --arg head "$branch" --arg base "$base" --arg title "$title" --arg body "$body" \
		--argjson draft "$draft" \
		'{head: $head, base: $base, title: $title, body: $body, draft: $draft}')
	resp=$(api_call POST "/repos/$repo/pulls" "$payload")
fi
printf '%s' "$resp" | jq -r '.html_url // empty'
