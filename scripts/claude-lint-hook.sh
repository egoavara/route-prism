#!/usr/bin/env bash
# Stop hook: auto-format with `moon run :format`, then surface unfixable
# lint violations from `moon run :lint` so Claude can address them.
set -u

ROOT="${CLAUDE_PROJECT_DIR:-$(git rev-parse --show-toplevel 2>/dev/null)}"
[ -n "${ROOT:-}" ] || exit 0
cd "$ROOT" || exit 0

if ! command -v moon >/dev/null 2>&1; then
  echo "moon CLI not found on PATH. Install: https://moonrepo.dev/docs/install" >&2
  exit 0
fi

# 1) Auto-format. Cache is disabled per-task in moon.yml so this always runs.
#    Discard stdout; surface errors only if the format step itself fails.
fmt_out=$(moon run :format 2>&1)
fmt_ec=$?
if [ $fmt_ec -ne 0 ]; then
  printf 'moon run :format failed (exit %s):\n\n%s\n' "$fmt_ec" "$fmt_out" >&2
  exit 2
fi

# 2) Lint check. Anything that survives format is something Claude must fix.
lint_out=$(moon run :lint 2>&1)
lint_ec=$?
if [ $lint_ec -ne 0 ]; then
  printf 'lint failed (exit %s). Fix the following before ending the turn:\n\n%s\n' \
    "$lint_ec" "$lint_out" >&2
  exit 2
fi
