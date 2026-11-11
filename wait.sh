#!/bin/sh
# Admission gate, run by the runner's pre_get_sources_script: drop a ticket, wait for the
# regulator's go file. Never blocks CI: passes when the regulator is silent or after GATE_MAX_WAIT.
id="${CI_JOB_ID:-}"
[ -n "$id" ] || exit 0
g="${GATE_DIR:-/gate}"
printf 'project=%s\nname=%s\n' "$CI_PROJECT_ID" "$CI_JOB_NAME" > "$g/waiting/$id" || exit 0
start=$(date +%s)
while [ ! -e "$g/go/$id" ]; do
  now=$(date +%s)
  alive=$(stat -c %Y "$g/alive" 2>/dev/null || echo 0)
  if [ $((now - alive)) -gt 30 ]; then echo "gate: regulator silent, passing"; break; fi
  if [ $((now - start)) -gt "${GATE_MAX_WAIT:-1200}" ]; then echo "gate: max wait reached, passing"; break; fi
  sleep 1
done
: > "$g/go/$id"
echo "gate: admitted after $(($(date +%s) - start))s"
