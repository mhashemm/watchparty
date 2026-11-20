#!/bin/sh
# Deploys the relay at master's commit $1. webhook runs it (hooks.json) when the
# deploy-relay workflow calls; its output is the workflow's log, and its last line
# "deployed <sha>" is how the workflow knows it worked.
set -eu

# Everything runs inside main, so the shell has read the whole script before the
# checkout below replaces this file.
main() {
	sha=${1:-}
	case $sha in
	*[!0-9a-f]* | "") echo "not a commit: $sha" >&2; exit 1 ;;
	esac
	[ ${#sha} -eq 40 ] || { echo "not a full commit: $sha" >&2; exit 1; }

	cd "$(dirname "$0")/../.."
	exec 9>"$(git rev-parse --git-dir)/relay-deploy.lock"
	flock -n 9 || { echo "another deploy is running" >&2; exit 1; }

	git fetch --quiet origin master
	git merge-base --is-ancestor "$sha" origin/master || { echo "$sha is not on master" >&2; exit 1; }
	git diff --quiet HEAD || { echo "the checkout has local changes:" >&2; git status --short >&2; exit 1; }
	if git rev-parse --quiet --verify refs/heads/master >/dev/null; then
		[ -z "$(git rev-list origin/master..master)" ] || { echo "local master has commits that aren't on origin" >&2; exit 1; }
	fi
	# -B also moves master back, so re-running an old deploy-relay run rolls back
	git checkout --quiet -B master "$sha"

	compose="docker compose -f relay/compose.yaml"
	$compose up -d --build --wait --wait-timeout 30 || { $compose logs --tail 20 relay >&2; exit 1; }
	# a request without a room is refused with 400 by a relay that is up
	addr=$($compose port relay 80)
	tries=0
	until [ "$(curl -s -o /dev/null -w '%{http_code}' "http://$addr/ws")" = 400 ]; do
		tries=$((tries + 1))
		[ $tries -lt 10 ] || { echo "the relay doesn't answer on $addr" >&2; $compose logs --tail 20 relay >&2; exit 1; }
		sleep 1
	done
	echo "deployed $sha"
}

main "$@"
