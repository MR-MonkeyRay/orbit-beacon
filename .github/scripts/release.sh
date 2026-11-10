#!/usr/bin/env bash
# One build, SHA staging, verified Release assets, then promotion of the same index.
set -euo pipefail

: "${VERSION:?}" "${GITHUB_SHA:?}" "${GITHUB_REPOSITORY:?}" "${RUNNER_TEMP:?}"
image="ghcr.io/${GITHUB_REPOSITORY,,}"
tag="v$VERSION"
work="$(mktemp -d "$RUNNER_TEMP/publication.XXXXXX")"
output="$work/binaries"
mkdir -p "$output"
container=
cleanup() {
  if [ -n "$container" ]; then docker rm "$container" >/dev/null; fi
  rm -rf "$work"
}
trap cleanup EXIT

conflict() {
  echo "Publication conflict: $*. Restore from the original commit or increment VERSION; nothing will be overwritten." >&2
  exit 1
}

# A missing object is different from an authentication/network failure.
inspect_index() {
  local ref=$1 destination=$2
  if docker buildx imagetools inspect "$ref" --format '{{json .Manifest}}' > "$destination" 2> "$work/image-error"; then
    return 0
  fi
  if grep -Eqi 'manifest unknown|not found|name unknown' "$work/image-error"; then return 1; fi
  cat "$work/image-error" >&2
  exit 1
}

read_release() {
  release_exists=false
  # The by-tag REST endpoint excludes drafts; authenticated listing includes them.
  gh api --paginate --slurp "repos/$GITHUB_REPOSITORY/releases?per_page=100" > "$work/releases.json"
  jq --arg tag "$tag" '[.[][] | select(.tag_name == $tag)] | if length > 1 then error("duplicate release tag") elif length == 1 then .[0] else {draft:true,assets:[]} end' \
    "$work/releases.json" > "$work/release.json"
  if jq -e --arg tag "$tag" '.tag_name == $tag' "$work/release.json" >/dev/null; then release_exists=true; fi
}

names=()
for arch in amd64 arm64; do
  for kind in server agent mcp; do names+=("orbitbeacon-$kind-linux-$arch"); done
 done

# ls-remote checks dangling tags too; fetching and peeling handles annotated tags.
remote_tag="$(git ls-remote origin "refs/tags/$tag")"
source_sha=
if [ -n "$remote_tag" ]; then
  git fetch --quiet --no-tags origin "refs/tags/$tag"
  source_sha="$(git rev-parse 'FETCH_HEAD^{commit}')"
fi
read_release
if [ "$release_exists" = true ] && [ -z "$source_sha" ]; then conflict 'Release has no source tag'; fi
expected_sha="${source_sha:-$GITHUB_SHA}"

version_exists=false
stage_exists=false
if inspect_index "$image:$VERSION" "$work/version.json"; then version_exists=true; fi
if inspect_index "$image:$expected_sha" "$work/stage.json"; then stage_exists=true; fi
index=
if [ "$version_exists" = true ]; then index="$work/version.json"; fi
if [ "$stage_exists" = true ]; then
  if [ -n "$index" ] && [ "$(jq -r '.digest' "$index")" != "$(jq -r '.digest' "$work/stage.json")" ]; then
    conflict 'version and SHA images have different manifests'
  fi
  index="$work/stage.json"
fi

build_date=
digest=
if [ -n "$index" ]; then
  digest="$(jq -er '.digest | select(test("^sha256:[0-9a-f]{64}$"))' "$index")"
  for arch in amd64 arm64; do
    platform_digest="$(jq -er --arg arch "$arch" '[.manifests[] | select(.platform.os == "linux" and .platform.architecture == $arch)] | if length == 1 then .[0].digest else error("missing or duplicate platform") end' "$index")"
    docker buildx imagetools inspect "$image@$platform_digest" --format '{{json .Image}}' > "$work/config-$arch.json"
    revision="$(jq -er '.config.Labels["org.opencontainers.image.revision"]' "$work/config-$arch.json")"
    image_version="$(jq -er '.config.Labels["org.opencontainers.image.version"]' "$work/config-$arch.json")"
    created="$(jq -er '.config.Labels["org.opencontainers.image.created"]' "$work/config-$arch.json")"
    if [ "$revision" != "$expected_sha" ] || [ "$image_version" != "$VERSION" ]; then
      conflict "$arch image source/version differs from tag"
    fi
    [ -z "$build_date" ] || [ "$build_date" = "$created" ] || conflict 'platform build dates differ'
    build_date="$created"
    printf '%s\n' "$platform_digest" > "$work/digest-$arch"
  done
fi

if [ "$release_exists" = true ] && [ "$(jq '.assets | length' "$work/release.json")" -gt 0 ]; then
  gh release download "$tag" --dir "$output"
fi

verify_binary() {
  local file=$1 revision created
  revision="$(go version -m "$file" | grep -oE 'vcs.revision=[0-9a-f]+' | cut -d= -f2)"
  [ "$revision" = "$expected_sha" ] || conflict "${file##*/} source commit differs from tag"
  # -trimpath omits ldflags from go version -m; linker -X strings are NUL-terminated.
  python3 - "$file" > "$work/binary-strings" <<'PY'
import re
import sys

with open(sys.argv[1], "rb") as binary:
    for match in re.finditer(rb"[\x20-\x7e]{4,}", binary.read()):
        print(match.group().decode("ascii"))
PY
  grep -Fxq "$expected_sha" "$work/binary-strings" || conflict "${file##*/} embedded commit differs"
  grep -Fxq "$VERSION" "$work/binary-strings" || conflict "${file##*/} embedded version differs"
  created="$(grep -E '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$' "$work/binary-strings" | sort -u)"
  [ -n "$created" ] || conflict "${file##*/} has no build date"
  [ -z "$build_date" ] || [ "$build_date" = "$created" ] || conflict "${file##*/} build date differs"
  build_date="$created"
}

# Trust neither asset names nor labels alone: check binary provenance as well.
for name in "${names[@]}"; do
  if [ -f "$output/$name" ]; then verify_binary "$output/$name"; fi
done

verify_sums() {
  local name checksum entry
  for name in "${names[@]}"; do
    entry="$(grep -E "^[0-9a-f]{64}  $name\$" "$output/SHA256SUMS")" || conflict "checksum missing for $name"
    [ "$(printf '%s\n' "$entry" | wc -l)" -eq 1 ] || conflict "duplicate checksum for $name"
    if [ -f "$output/$name" ]; then
      checksum="$(sha256sum "$output/$name")"
      [ "${checksum%% *}" = "${entry%% *}" ] || conflict "$name checksum differs"
    fi
  done
}
if [ -f "$output/SHA256SUMS" ]; then verify_sums; fi

complete=true
for name in "${names[@]}" SHA256SUMS; do
  if [ ! -f "$output/$name" ]; then complete=false; fi
 done
if [ "$version_exists" != true ] || [ "$(jq -r '.draft' "$work/release.json")" != false ]; then complete=false; fi
if [ "$complete" != true ] && [ "$expected_sha" != "$GITHUB_SHA" ]; then
  conflict "partial $tag belongs to $expected_sha, not $GITHUB_SHA"
fi

# Read the two Server files without running either architecture's entrypoint.
if [ -n "$index" ]; then
  for arch in amd64 arm64; do
    platform_digest="$(< "$work/digest-$arch")"
    docker pull --platform "linux/$arch" "$image@$platform_digest" >/dev/null
    container="$(docker create --platform "linux/$arch" "$image@$platform_digest")"
    docker cp "$container:/usr/local/bin/orbitbeacon-server" "$work/server-$arch"
    verify_binary "$work/server-$arch"
    docker rm "$container" >/dev/null
    container=
    name="orbitbeacon-server-linux-$arch"
    if [ -f "$output/$name" ]; then
      cmp "$output/$name" "$work/server-$arch" || conflict "$arch Release Server differs from image"
    else
      cp "$work/server-$arch" "$output/$name"
    fi
  done
fi

if [ "$complete" = true ]; then
  echo "Verified complete $tag from $expected_sha; skipping publication"
  exit 0
fi

build_date="${build_date:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
for arch in amd64 arm64; do
  for kind in server agent mcp; do
    name="orbitbeacon-$kind-linux-$arch"
    if [ ! -f "$output/$name" ]; then
      CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath \
        -ldflags "-s -w -X orbitbeacon/internal/buildinfo.Version=$VERSION -X orbitbeacon/internal/buildinfo.Commit=$GITHUB_SHA -X orbitbeacon/internal/buildinfo.BuildDate=$build_date" \
        -o "$output/$name" "./cmd/orbitbeacon-$kind"
    fi
  done
 done
if [ -f "$output/SHA256SUMS" ]; then
  verify_sums
else
  (cd "$output" && sha256sum orbitbeacon-*-linux-* > SHA256SUMS)
fi

if [ -z "$index" ]; then
  docker buildx build --file build/server.Dockerfile --target precompiled \
    --build-context "release-binaries=$output" --platform linux/amd64,linux/arm64 \
    --provenance=true --sbom=true --push --tag "$image:$GITHUB_SHA" \
    --build-arg VERSION="$VERSION" --build-arg COMMIT="$GITHUB_SHA" --build-arg BUILD_DATE="$build_date" .
  inspect_index "$image:$GITHUB_SHA" "$work/built.json"
  digest="$(jq -er '.digest | select(test("^sha256:[0-9a-f]{64}$"))' "$work/built.json")"
fi

if [ -z "$source_sha" ]; then
  gh api "repos/$GITHUB_REPOSITORY/git/refs" --method POST -f "ref=refs/tags/$tag" -f "sha=$GITHUB_SHA" >/dev/null
fi
if [ "$release_exists" != true ]; then
  gh release create "$tag" --verify-tag --draft --title "$VERSION" \
    --notes "OrbitBeacon $VERSION release. See SHA256SUMS for checksums."
fi
# Existing bytes were verified above; never clobber even on a same-SHA recovery.
for name in "${names[@]}" SHA256SUMS; do
  if ! jq -e --arg name "$name" '.assets[] | select(.name == $name)' "$work/release.json" >/dev/null; then
    gh release upload "$tag" "$output/$name"
  fi
 done

# Re-download the remote assets before exposing a deployable image tag.
mkdir "$work/published"
gh release download "$tag" --dir "$work/published"
for name in "${names[@]}" SHA256SUMS; do
  cmp "$output/$name" "$work/published/$name" || conflict "uploaded $name differs"
 done
gh release edit "$tag" --draft=false
read_release
[ "$(jq -r '.draft' "$work/release.json")" = false ] || conflict 'Release is still a draft'
# Publication changes draft download URLs; verify the final public asset set too.
mkdir "$work/public"
gh release download "$tag" --dir "$work/public"
for name in "${names[@]}" SHA256SUMS; do
  cmp "$output/$name" "$work/public/$name" || conflict "public $name differs"
done

# Version is the completion marker; interruption after latest is safe to resume.
docker buildx imagetools create --tag "$image:latest" "$image@$digest"
docker buildx imagetools create --tag "$image:$VERSION" "$image@$digest"
