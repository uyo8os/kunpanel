#!/usr/bin/env bash
set -euo pipefail

# Fetch into a private local ref so annotated tags are peeled without changing tags.
set +e
git ls-remote --exit-code --refs origin "refs/tags/$TAG" > /dev/null
tag_status=$?
set -e
case "$tag_status" in
  0)
    git fetch --no-tags origin "+refs/tags/$TAG:refs/release-check/tag"
    tag_commit="$(git rev-parse 'refs/release-check/tag^{commit}')"
    if [[ "$tag_commit" != "$GITHUB_SHA" ]]; then
      echo "::error::Tag $TAG points to $tag_commit, not selected commit $GITHUB_SHA."
      exit 1
    fi
    tag_exists=true
    ;;
  2) tag_exists=false ;;
  *) echo "::error::Could not check remote tag $TAG."; exit 1 ;;
esac

if [[ "${SKIP_RELEASE_CHECK:-false}" != true ]]; then
  # The tag-specific API only returns published releases; list all pages to
  # detect drafts as well. A failed API request must not be treated as absence.
  if ! release_tags="$(gh api --paginate "repos/$GITHUB_REPOSITORY/releases?per_page=100" --jq '.[].tag_name')"; then
    echo "::error::Could not check whether the release exists."
    exit 1
  fi
  if grep -Fxq -- "$TAG" <<< "$release_tags"; then
    echo "::error::A release for $TAG already exists; remove or resolve it before retrying."
    exit 1
  fi
fi

RELEASE_TAG_EXISTS="$tag_exists"
echo "Tag $TAG exists on remote: $tag_exists"
