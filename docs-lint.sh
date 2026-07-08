#!/usr/bin/env bash
# docs-lint: mechanical guard against index-smell and duplication in Markdown docs.
#
# The filesystem is the sole source of location truth, so docs must not enumerate
# file/dir/symbol locations, must not restate a fact that has a canonical home, and
# must not reference renamed or removed paths. This check encodes those rules so they
# are caught on every change instead of by inspection.
#
# Scope: every tracked *.md except generated .agents/skills/*/SKILL.md and the
# CLAUDE.md symlink. Run from the repo root. Exit non-zero on any violation.

set -uo pipefail

repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root" || exit 2

fail=0
note() { printf '%s\n' "$*"; }
flag() { fail=1; printf 'FAIL: %s\n' "$*"; }

# Editable docs: tracked *.md minus generated skills and the CLAUDE.md symlink.
mapfile -t docs < <(git ls-files '*.md' \
  | grep -vE '^\.agents/skills/.*/SKILL\.md$' \
  | grep -vE '^CLAUDE\.md$')

# ---------------------------------------------------------------------------
# 1. Banned index-table headers.
# ---------------------------------------------------------------------------
hits="$(grep -rInE '^\|[[:space:]]*(Path|File|Files|Location|Dir|Directory)[[:space:]]*\|' "${docs[@]}" 2>/dev/null)"
if [[ -n "$hits" ]]; then flag "index-table header (| Path |/| File |/...)"; printf '%s\n' "$hits"; fi

# ---------------------------------------------------------------------------
# 2. Table rows that are file-link indexes (row's first cell is a link).
# ---------------------------------------------------------------------------
hits="$(grep -rInE '^\|[[:space:]]*\[' "${docs[@]}" 2>/dev/null)"
if [[ -n "$hits" ]]; then flag "file-link index table row (| [..](..) | ...)"; printf '%s\n' "$hits"; fi

# ---------------------------------------------------------------------------
# 3. Enumeration section headings that list locations instead of behavior.
# ---------------------------------------------------------------------------
hits="$(grep -rInE '^#{1,6}[[:space:]]+(Repo layout|Topics index|Operational pointers|Sources|Sources of truth|Components|Files)[[:space:]]*$' "${docs[@]}" 2>/dev/null)"
if [[ -n "$hits" ]]; then flag "enumeration section heading"; printf '%s\n' "$hits"; fi

# ---------------------------------------------------------------------------
# 4. Em/en dashes.
# ---------------------------------------------------------------------------
hits="$(grep -rInP '\xe2\x80\x93|\xe2\x80\x94' "${docs[@]}" 2>/dev/null)"
if [[ -n "$hits" ]]; then flag "em/en dash"; printf '%s\n' "$hits"; fi

# ---------------------------------------------------------------------------
# 5. Stale / renamed / removed path fragments (docs + source comments).
# ---------------------------------------------------------------------------
mapfile -t stale_targets < <(git ls-files '*.md' 'mwan/go/**/*.go' 'ansible/**/*.yml' 'ansible/**/*.j2' 'mwan/**/*.j2' 'README.md' 'AGENTS.md' 2>/dev/null)
hits="$(grep -rInE 'mwan/config/config\.toml\.j2|internal/mwn1|modules/wghealth|mwan-layout\.md|suburban-testbed\.md|operational-notes\.md|config-import\.md|testbed-baseline\.md|testbed-config-import\.md|testbed-dns-nat64\.md|ui-testing\.md|wireguard-roaming\.md|dscp-wan-pinning\.md|go-standards\.md|script-style\.md|proxmox-api\.md|mwan-email-routing' "${stale_targets[@]}" 2>/dev/null)"
if [[ -n "$hits" ]]; then flag "stale/renamed path fragment"; printf '%s\n' "$hits"; fi

# ---------------------------------------------------------------------------
# 6. Duplicated canonical rule phrasings (each must appear in exactly one doc).
# ---------------------------------------------------------------------------
check_singleton() {
  local label="$1" pattern="$2"
  local files count
  files="$(grep -rIlE "$pattern" "${docs[@]}" 2>/dev/null)"
  count="$(printf '%s' "$files" | grep -c . )"
  if [[ "$count" -gt 1 ]]; then
    flag "duplicated canonical rule: $label in $count docs"
    printf '%s\n' "$files"
  fi
}
# The no-saved-RAM snapshot rationale (the "why", not a pointer to it).
check_singleton "vmstate-no-RAM-rationale" 'includes RAM resumes on rollback|resumes on rollback with a stale'
# The default()/is defined ban rationale in docs (AGENTS.md contract is allowed once; quality.md is the reference).
check_singleton "default-ban-rationale" 'a missing value fails at load time'
# The vault_* naming-contract statement.
check_singleton "vault-naming-contract" 'every secret name starts with|under a `vault_\*` name|secret values live in Ansible Vault under'

# ---------------------------------------------------------------------------
# 7. Broken relative markdown links.
# ---------------------------------------------------------------------------
broken=0
for f in "${docs[@]}" AGENTS.md README.md; do
  [[ -e "$f" ]] || continue
  dir="$(dirname "$f")"
  while IFS= read -r target; do
    [[ -z "$target" ]] && continue
    case "$target" in http*|mailto:*|\#*) continue ;; esac
    base="${target%%#*}"; [[ -z "$base" ]] && continue
    if [[ ! -e "$dir/$base" ]]; then flag "broken link: $f -> $target"; broken=1; fi
  done < <(grep -oE '\]\([^)]+\)' "$f" 2>/dev/null | sed -E 's/^\]\(//; s/\)$//')
done
[[ "$broken" -eq 0 ]] || true

if [[ "$fail" -eq 0 ]]; then
  note "docs-lint: OK (no index-smell, duplication, stale-path, dash, or broken-link violations)"
fi
exit "$fail"
