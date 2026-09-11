#!/bin/sh
# Check that the terminal scripts reject missing output on both timeout and EOF.
# The probe here is deliberately inert; the real picker is tested separately.

set -eu

work=$(mktemp -d "${TMPDIR:-/tmp}/pimctl-tui-harness.XXXXXX")
trap 'rm -rf "$work"' EXIT HUP INT TERM

cat >"$work/probe" <<'EOF'
#!/bin/sh
if [ "$PIMCTL_TUI_PROBE_MODE" = timeout ]; then
  exec sleep 3
fi
exit 0
EOF
chmod 700 "$work/probe"

for script in scripts/tui-*-test.exp; do
  for mode in timeout eof; do
    if PIMCTL_TUI_TIMEOUT=1 PIMCTL_TUI_PROBE_MODE="$mode" \
      expect "$script" "$work/probe" >"$work/output" 2>&1; then
      printf '%s accepted a probe with no output (%s)\n' "$script" "$mode" >&2
      exit 1
    fi
    if ! grep -q '!!! FAIL:' "$work/output"; then
      cat "$work/output" >&2
      printf '%s failed outside its assertion handler (%s)\n' "$script" "$mode" >&2
      exit 1
    fi
  done
done
printf 'Terminal harness rejects missing output on timeout and EOF.\n'
