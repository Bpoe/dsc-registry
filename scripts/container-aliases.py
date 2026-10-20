"""Select non-regressing aliases from published releases while the publisher holds its concurrency lock."""

import json
from pathlib import Path
import re
import sys


def stable_version(tag):
    match = re.fullmatch(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", tag)
    return tuple(map(int, match.groups())) if match else None


def aliases(tag, releases):
    version = stable_version(tag)
    if version is None:
        raise ValueError("Expected a stable vMAJOR.MINOR.PATCH tag.")
    versions = [
        parsed for release in releases
        if not release["draft"] and not release["prerelease"]
        and (parsed := stable_version(release["tag_name"])) is not None
    ]
    if version not in versions:
        raise ValueError("The triggering stable release is no longer published.")
    result = []
    if version == max(v for v in versions if v[:2] == version[:2]):
        result.append(f"{version[0]}.{version[1]}")
    if version == max(versions):
        result.append("latest")
    return result


if __name__ == "__main__":
    # gh api --paginate --slurp produces one release array per page.
    pages = json.loads(Path(sys.argv[2]).read_text(encoding="utf-8"))
    for alias in aliases(sys.argv[1], [release for page in pages for release in page]):
        print(alias)
