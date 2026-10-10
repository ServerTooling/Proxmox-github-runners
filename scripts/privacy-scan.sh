#!/usr/bin/env bash
# Fails if anything personal could be published: private/LAN IP addresses, home-network hostnames,
# email addresses, or credentials. Runs over the source tree on every PR and over every release file
# (including the readable strings inside binaries) before publishing. This repo is public.
#
# Usage: scripts/privacy-scan.sh [PATH...]   (default: the git tree)
set -euo pipefail

# Documentation ranges (RFC 5737) are fine.
# Network addresses (x.0.0.0-style) identify a range, never a machine, so they may appear anywhere.
ALLOW='\b(192\.0\.2\.[0-9]+|198\.51\.100\.[0-9]+|203\.0\.113\.[0-9]+|169\.254\.169\.254|(10\.0\.0\.0|172\.16\.0\.0|192\.168\.0\.0|100\.64\.0\.0|169\.254\.0\.0)(/[0-9]{1,2})?)'
PATTERNS=(
  '\b10\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\b'
  '\b172\.(1[6-9]|2[0-9]|3[01])\.[0-9]{1,3}\.[0-9]{1,3}\b'
  '\b192\.168\.[0-9]{1,3}\.[0-9]{1,3}\b'
  '\b100\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\.[0-9]{1,3}\.[0-9]{1,3}\b'
  '\b[a-z0-9-]+\.(lan|home|localdomain|home\.arpa|fritz\.box)\b'
  '\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b'
  '\b(ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36}\b'
  'github_pat_[A-Za-z0-9_]{40,}'
  '-----BEGIN [A-Z ]*PRIVATE KEY-----'
)
# Inside binaries, random byte runs like "H@oa.nF" look like emails; real addresses there have a
# lowercase domain and a local part of two or more characters, so binaries use this stricter form.
BINARY_EMAIL='\b[A-Za-z0-9][A-Za-z0-9._%+-]+@[a-z0-9][a-z0-9-]+(\.[a-z0-9-]+)*\.[a-z]{2,24}\b'
# Emails that are fine to publish.
EMAIL_OK='(noreply@anthropic\.com|noreply@github\.com|@example\.(com|org|net)|@users\.noreply\.github\.com)'

scan_stream() { # name, binary (0|1), then text on stdin
  local name=$1 binary=$2 text hits=0
  text=$(sed -E "s#${ALLOW}#<ok>#g")
  for p in "${PATTERNS[@]}"; do
    if [ "$binary" = 1 ] && [[ $p == *@* ]]; then p=$BINARY_EMAIL; fi
    if m=$(grep -nEo -e "$p" <<<"$text" | grep -Ev "$EMAIL_OK"); then
      while IFS= read -r line; do echo "$name:$line"; done <<<"$m"
      hits=1
    fi
  done
  return $hits
}

status=0
if [ $# -eq 0 ]; then
  mapfile -t files < <(git ls-files)
else
  files=()
  for p in "$@"; do
    if [ -d "$p" ]; then mapfile -t -O "${#files[@]}" files < <(find "$p" -type f); else files+=("$p"); fi
  done
fi
for f in "${files[@]}"; do
  [ -f "$f" ] || continue
  case "$f" in scripts/privacy-scan.sh) continue ;; esac
  if grep -Iq . "$f" 2>/dev/null || [ ! -s "$f" ]; then
    scan_stream "$f" 0 <"$f" || status=1
  else
    strings -n 6 "$f" | scan_stream "$f" 1 || status=1
  fi
done
if [ $status -ne 0 ]; then
  echo "privacy-scan: personal or secret data found above; remove it before publishing" >&2
else
  echo "privacy-scan: clean (${#files[@]} files)"
fi
exit $status
