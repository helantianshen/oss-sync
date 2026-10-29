#!/usr/bin/env python3
"""检查客户端版本与兼容映射，允许根清单保留上一正式版公告"""
import argparse
import json
from pathlib import Path
import re


# 检查以源码目录为根，供 CI 和本地从任意工作目录调用
ROOT = Path(__file__).resolve().parent.parent


def check_versions(release: str = "") -> str:
    """校验构建版本、锁文件与兼容表，不提前推进市场公告"""
    paths = (
        "manifest.json", "versions.json", "plugin/manifest.json",
        "plugin/package.json", "plugin/package-lock.json", "plugin/versions.json",
    )
    data = {path: json.loads((ROOT / path).read_text(encoding="utf-8")) for path in paths}
    manifest = data["plugin/manifest.json"]
    current = manifest["version"]
    if not re.fullmatch(r"\d+\.\d+\.\d+(?:-rc\.\d+)?", current):
        raise SystemExit(f"plugin/manifest.json: invalid release version {current!r}")
    if release and release.removeprefix("v") != current:
        raise SystemExit(f"release {release} does not match plugin version {current}")
    for path, actual in (
        ("plugin/package.json", data["plugin/package.json"]["version"]),
        ("plugin/package-lock.json", data["plugin/package-lock.json"]["version"]),
        ("plugin/package-lock.json packages[root]", data["plugin/package-lock.json"]["packages"][""]["version"]),
    ):
        if actual != current:
            raise SystemExit(f"{path}: {actual} does not match plugin version {current}")
    if data["versions.json"] != data["plugin/versions.json"]:
        raise SystemExit("plugin/versions.json must match versions.json")
    for path in ("manifest.json", "plugin/manifest.json"):
        entry = data[path]
        if entry["id"] != "oss-sync":
            raise SystemExit(f"{path}: plugin id must be oss-sync")
        # RC 不进入市场兼容表；根清单作为市场公告，必须指向已有映射的正式版
        if path == "manifest.json" or "-rc." not in entry["version"]:
            if data["versions.json"].get(entry["version"]) != entry["minAppVersion"]:
                raise SystemExit(f"{path}: version/minAppVersion missing or inconsistent in versions.json")
    return current


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--release", default="", help="Release tag, including optional v prefix")
    args = parser.parse_args()
    print(f"Version metadata consistent: {check_versions(args.release)}")
