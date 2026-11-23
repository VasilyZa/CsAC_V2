#!/usr/bin/env bash
set -euo pipefail

payload=${1:?Provide a release archive}
: "${CSAC_DEPLOY_HOST:?Set CSAC_DEPLOY_HOST in the production environment}"
: "${CSAC_DEPLOY_PORT:?Set CSAC_DEPLOY_PORT in the production environment}"
: "${CSAC_DEPLOY_USER:?Set CSAC_DEPLOY_USER in the production environment}"
: "${CSAC_DEPLOY_KEY:?Set CSAC_DEPLOY_KEY in the production environment}"
: "${CSAC_DEPLOY_KNOWN_HOSTS:?Set CSAC_DEPLOY_KNOWN_HOSTS in the production environment}"

credentials=$(mktemp -d)
trap 'rm -rf -- "$credentials"' EXIT
umask 077
printf '%s\n' "$CSAC_DEPLOY_KEY" > "$credentials/key"
printf '%s\n' "$CSAC_DEPLOY_KNOWN_HOSTS" > "$credentials/known_hosts"
unset CSAC_DEPLOY_KEY CSAC_DEPLOY_KNOWN_HOSTS

size=$(stat --format='%s' -- "$payload")
checksum=$(sha256sum < "$payload")
{
  printf 'release %s %s %s %s\n' "$GITHUB_SHA" "$GITHUB_RUN_NUMBER" "$size" "${checksum%% *}"
  cat -- "$payload"
} | ssh -F /dev/null -T -i "$credentials/key" -p "$CSAC_DEPLOY_PORT" \
  -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=yes \
  -o "UserKnownHostsFile=$credentials/known_hosts" -o GlobalKnownHostsFile=/dev/null \
  -o ClearAllForwardings=yes -o ConnectTimeout=15 -o ServerAliveInterval=15 -o ServerAliveCountMax=4 \
  -- "$CSAC_DEPLOY_USER@$CSAC_DEPLOY_HOST"
