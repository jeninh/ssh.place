#!/usr/bin/env bash
#
# Pull main and restart. This is the only thing the deploy key is allowed to run:
# it is wired into authorized_keys as a forced command, so a leaked key gets you
# a redeploy and nothing else. See the "Automatic deploys" section of the README.
#
# Runs unattended from CI, so it fails loudly rather than half-deploying.
set -Eeuo pipefail

REPO="${SSHPLACE_REPO:-/root/ssh.place}"
BRANCH="${SSHPLACE_BRANCH:-main}"

log() { printf '[deploy] %s\n' "$*"; }
die() { printf '[deploy] FAILED: %s\n' "$*" >&2; exit 1; }

[ -d "$REPO/.git" ] || die "$REPO is not a git checkout (set SSHPLACE_REPO)"
cd "$REPO"

before=$(git rev-parse --short HEAD)

log "fetching $BRANCH"
git fetch --quiet origin "$BRANCH" || die "git fetch"

# Refuse to deploy on top of local edits: silently discarding somebody's hotfix
# is worse than not deploying.
if ! git diff --quiet || ! git diff --cached --quiet; then
	die "working tree has uncommitted changes, refusing to overwrite them"
fi

git checkout --quiet "$BRANCH" || die "git checkout $BRANCH"
git reset --hard --quiet "origin/$BRANCH" || die "git reset"

after=$(git rev-parse --short HEAD)
if [ "$before" = "$after" ]; then
	log "already at $after, nothing to do"
else
	log "$before -> $after"
fi

log "building and restarting"
docker compose up -d --build --remove-orphans || die "docker compose up"

# Prove it actually came back rather than assuming a successful `up` means a
# working server.
#
# This reads the container's own healthcheck, which compose already runs, rather
# than curling a port. The app's HTTP port is deliberately not published to the
# host (only Caddy reaches it), and this way the check needs no curl on the host
# and does not depend on DNS or TLS being up.
log "waiting for health"
for i in $(seq 1 30); do
	status=$(docker inspect --format '{{.State.Health.Status}}' sshplace 2>/dev/null || echo missing)
	case "$status" in
	healthy)
		log "healthy at $after"
		# Old images pile up fast when every push builds a new one.
		docker image prune -f >/dev/null 2>&1 || true
		exit 0
		;;
	unhealthy)
		break
		;;
	esac
	sleep 2
done

log "unhealthy after 60s, recent logs:"
docker compose logs --tail 40 sshplace >&2 || true
die "health check never passed"
