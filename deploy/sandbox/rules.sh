# rules.sh — project-rule extraction for the code-review verify/cite feature (manyforge-8qs.2).
#
# emit_project_rules <workdir> writes to stdout a "project rules" block assembled from the
# reviewed repo's OWN rule docs, so a review can cite the project's conventions (rule_id).
#
# It reads ONLY three fixed, well-known doc paths — never a glob, never a directory walk — so it
# cannot pull a secret file (.env, credentials) into the model prompt. The combined output is
# byte-capped. When none of the docs exist it prints nothing (exit 0) and the caller skips seeding.
#
# Sourced by entrypoint.sh; unit-tested directly via bash (entrypoint_rules_test.go) so the
# doc-reading logic is validated without opencode or Docker.

# RULES_MAX_BYTES caps the combined rules block so it can't crowd out the diff in the prompt.
: "${RULES_MAX_BYTES:=16384}"

emit_project_rules() {
  work="${1:-/work}"
  # Fixed allowlist of rule-doc paths, in priority order. NOT globbed — reading only these exact
  # relative paths is what keeps a secret file out of the prompt.
  set -- "CLAUDE.md" ".specify/memory/constitution.md" "AGENTS.md"
  found=""
  body=""
  for rel in "$@"; do
    f="${work}/${rel}"
    # -f guards against a path that is a directory/symlink-to-dir; read regular files only.
    if [ -f "$f" ]; then
      found="yes"
      body="${body}
=== ${rel} ===
$(cat "$f")
"
    fi
  done
  [ -n "$found" ] || return 0

  printf '%s\n' "PROJECT RULES (from the reviewed repository's own docs). When a finding relates to one of these rules, set its \"rule_id\" to a short free-form citation (e.g. the rule's heading or a few words identifying it). Only cite a rule that genuinely applies; leave rule_id empty otherwise."
  # Byte-cap the concatenated docs (head -c is a hard byte bound — safe for the prompt budget).
  printf '%s' "$body" | head -c "$RULES_MAX_BYTES"
  printf '\n'
}
