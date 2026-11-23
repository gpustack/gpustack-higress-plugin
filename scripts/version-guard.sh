#!/usr/bin/env bash
# version-guard.sh — enforce the AGENTS.md plugin versioning policy:
# between two consecutive repository tags, each plugin bumps its version
# at most once, and a plugin whose sources did not change must not bump.
#
# Compares the git tree against BASE_REF (default: the most recent tag
# reachable from HEAD). For every extensions/<name>/VERSION:
#   - sources changed (diff excludes VERSION and README variants) -> the
#     version must differ from the one at BASE_REF, must be HIGHER under
#     SemVer precedence (prerelease < release; build metadata ignored),
#     and must have been bumped in at most ONE commit on the range;
#   - sources unchanged -> the version must be IDENTICAL.
# New plugins (no VERSION at BASE_REF) must simply have a valid version.
#
# SemVer comparison is delegated to python3: `sort -V` does not implement
# SemVer precedence (it orders `1.0.0-rc.1` above `1.0.0`, which would
# reject a valid prerelease promotion and accept the reverse downgrade).
#
# NOTE: the commit-count check sees the branch's own history, so bumps
# squashed into one commit by amend/rebase are indistinguishable from a
# single bump — that residual is documented in AGENTS.md and left to
# review.
set -euo pipefail

# semver_valid V -> exit 0 only when V satisfies the complete SemVer
# grammar (official regex: no leading zeros in the core, non-empty
# dot-separated prerelease/build identifiers of [0-9A-Za-z-]). Used as
# the preflight so malformed versions like 01.0.0, 1.0.0-rc..1 or
# 1.0.0+@ fail before any early-success or comparison path.
semver_valid() {
  python3 - "$1" <<'PY'
import re, sys
SEMVER = re.compile(
    r'^(?P<major>0|[1-9]\d*)\.(?P<minor>0|[1-9]\d*)\.(?P<patch>0|[1-9]\d*)'
    r'(?:-(?P<pre>(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)'
    r'(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?'
    r'(?:\+(?P<build>[0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$'
)
sys.exit(0 if SEMVER.fullmatch(sys.argv[1].strip()) else 1)
PY
}

# semver_cmp A B -> prints -1 / 0 / 1; exits 2 when either side is not
# parseable SemVer.
semver_cmp() {
  python3 - "$1" "$2" <<'PY'
import re, sys
def key(v):
    m = re.fullmatch(r'(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?', v)
    if not m:
        return None
    core = tuple(int(x) for x in m.group(1, 2, 3))
    pre = m.group(4)
    if pre is None:
        return core + (1,)
    parts = []
    for p in pre.split('.'):
        parts.append((0, int(p), '') if p.isdigit() else (1, 0, p))
    return core + (0, tuple(parts))
a, b = key(sys.argv[1]), key(sys.argv[2])
if a is None or b is None:
    sys.exit(2)
print(-1 if a < b else (1 if a > b else 0))
PY
}

BASE_REF="${1:-}"
# TAG_REF (optional 2nd arg): the release-tag window base. When set, a
# plugin whose sources changed without a bump is still accepted if its
# version was ALREADY bumped inside this release window (version differs
# from the tag's) — a follow-up fix PR must not pay a second bump for
# the same window.
TAG_REF="${2:-}"
cd "$(git rev-parse --show-toplevel)"

if [[ -z "$BASE_REF" ]]; then
  BASE_REF="$(git describe --tags --abbrev=0 2>/dev/null || true)"
  if [[ -z "$BASE_REF" ]]; then
    echo "version-guard: no previous tag reachable from HEAD; nothing to compare"
    exit 0
  fi
fi

BASE_SHA="$(git rev-parse --verify "$BASE_REF^{commit}")" ||
  { echo "version-guard: bad base ref '$BASE_REF'" >&2; exit 1; }

TAG_SHA=""
if [[ -n "$TAG_REF" && "$TAG_REF" != "$BASE_REF" ]]; then
  TAG_SHA="$(git rev-parse --verify "$TAG_REF^{commit}" 2>/dev/null || true)"
fi

echo "version-guard: comparing against $BASE_REF ($BASE_SHA)"

# Exemptions: scripts/version-guard.exempt, one entry per line ('#'
# comments allowed), two forms:
#   name            — full skip, for plugins whose versioning genuinely
#                     cannot follow the policy (e.g. a vendored fork
#                     tracking an upstream version scheme);
#   name @ <ref>    — grandfather: ONLY the bump-count check ignores
#                     commits before <ref> (pre-policy history); every
#                     other check stays active. The entry auto-lapses
#                     once the release window base descends from <ref>
#                     (the guard prints a note, never fails) — no manual
#                     cleanup is ever required; removing a lapsed entry
#                     is optional housekeeping.
# Full skips print "exempt", grandfathers print a "note" line, so every
# bypass stays visible in the log.
exempt_file="$(git rev-parse --show-toplevel)/scripts/version-guard.exempt"
entries=""
if [[ -f "$exempt_file" ]]; then
  # Strip comments and trim each line's whitespace, preserving line
  # separators so entries stay separate (`tr -d` across the whole
  # stream would fuse "plugin-a\nplugin-b" into "plugin-aplugin-b").
  # `|| true`: sed exits 0 but the pipeline may see an empty stream,
  # which pipefail + set -e must not treat as fatal.
  entries="$(sed 's/#.*//' "$exempt_file" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//' | sed '/^$/d' || true)"
fi
# full_exempt NAME: plain "name" line (no grandfather anchor).
full_exempt() { printf '%s\n' "$entries" | grep -qx -- "$1"; }
# grandfather_ref NAME: the anchor of a "name @ <ref>" line, or empty.
grandfather_ref() {
  printf '%s\n' "$entries" | awk -v n="$1" '$1 == n && $2 == "@" { print $3; exit }'
}

fail=0
for version_file in extensions/*/VERSION; do
  plugin_dir="$(dirname "$version_file")"
  name="$(basename "$plugin_dir")"
  current="$(cat "$version_file")"

  if full_exempt "$name"; then
    echo "exempt $name: listed in scripts/version-guard.exempt"
    continue
  fi

  # Grandfather (name @ <ref>): while the comparison base is OLDER than
  # the anchor, the pre-policy history in between is ignored — the
  # effective base becomes the anchor itself, for source attribution
  # AND the bump count alike (every other check stays active). Once the
  # base descends from the anchor the entry has lapsed — the pre-policy
  # history has fallen out of the window — and the guard says so in a
  # note; no manual cleanup is ever required.
  eff_base="$BASE_SHA"
  grandfathered=0
  gref="$(grandfather_ref "$name")"
  if [[ -n "$gref" ]]; then
    gsha="$(git rev-parse --verify "$gref^{commit}" 2>/dev/null || true)"
    if [[ -z "$gsha" ]]; then
      echo "FAIL $name: grandfather anchor '$gref' does not resolve"
      fail=1
      continue
    elif git merge-base --is-ancestor "$gsha" "$BASE_SHA"; then
      echo "note $name: grandfather entry lapsed (base is past $gref); inert, removable"
    else
      eff_base="$gsha"
      grandfathered=1
      echo "note $name: grandfathered at $gref (pre-policy history ignored)"
    fi
  fi

  if ! semver_valid "$current"; then
    echo "FAIL $name: VERSION '$current' is not valid semver"
    fail=1
    continue
  fi

  base_version_file="$eff_base:$version_file"
  if ! git cat-file -e "$base_version_file" 2>/dev/null; then
    echo "ok   $name: new plugin at $current"
    continue
  fi
  base_version="$(git show "$base_version_file")"

  # Sources changed = anything under the plugin dir except VERSION itself
  # and the README variants (README.md, README_EN.md, README_dev.md, ...):
  # documentation is not plugin behavior and must not force a version
  # bump, so documentation-only PRs do not consume a between-release bump.
  if git diff --quiet "$eff_base" HEAD -- "$plugin_dir" \
      ':(exclude)'"$version_file" ':(exclude,glob)'"$plugin_dir/README*.md"; then
    if [[ "$current" != "$base_version" ]]; then
      echo "FAIL $name: sources unchanged vs $BASE_REF but VERSION moved '$base_version' -> '$current'"
      fail=1
    else
      echo "ok   $name: unchanged, version stays $current"
    fi
  else
    if [[ "$current" == "$base_version" ]]; then
      # Retention: sources changed without a bump, but the version was
      # already bumped inside this release window (a preceding PR in the
      # same window did it) — accepted, so a follow-up fix does not pay
      # a second bump for the same window.
      retained=0
      if [[ -n "$TAG_SHA" ]]; then
        tag_version="$(git show "$TAG_SHA:$version_file" 2>/dev/null || true)"
        if [[ -n "$tag_version" && "$tag_version" != "$current" ]]; then
          retained=1
        fi
      fi
      if [[ "$retained" == 1 ]]; then
        echo "ok   $name: $current (retains the version bumped earlier in this release window)"
      else
        echo "FAIL $name: sources changed vs $BASE_REF but VERSION stayed '$current'"
        fail=1
      fi
    else
      cmp_res="$(semver_cmp "$base_version" "$current")" || {
        echo "FAIL $name: cannot compare '$base_version' -> '$current' as semver"
        fail=1
        continue
      }
      if [[ "$cmp_res" != "-1" ]]; then
        echo "FAIL $name: VERSION not a SemVer step up '$base_version' -> '$current'"
        fail=1
        continue
      fi
      # At most one bump per tag window: count commits touching the
      # VERSION file across the WHOLE window (TAG..HEAD when a tag base
      # is supplied) — counting only BASE..HEAD would let a PR's own
      # single bump hide a bump the base branch already carries in the
      # same window, failing the subsequent push instead. A grandfathered
      # plugin counts from its anchor (eff_base) instead: the anchor is
      # newer than the tag, so the pre-policy bumps drop out. Merge
      # commits are skipped (their touching a file is an artifact of the
      # merge, not a decision).
      if [[ "$grandfathered" == 1 ]]; then
        count_base="$eff_base"
      else
        count_base="${TAG_SHA:-$BASE_SHA}"
      fi
      bump_commits="$(git rev-list --count --no-merges "$count_base..HEAD" -- "$version_file")"
      if [[ "$bump_commits" -gt 1 ]]; then
        echo "FAIL $name: VERSION bumped in $bump_commits separate commits on this range (policy: one bump per tag window)"
        fail=1
        continue
      fi
      echo "ok   $name: $base_version -> $current"
    fi
  fi
done

exit "$fail"
