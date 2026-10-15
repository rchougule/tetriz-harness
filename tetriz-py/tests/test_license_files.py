# Copyright 2026 Workloom Solutions Private Limited
# SPDX-License-Identifier: MIT

"""The licence files the tetriz-py image ships, and the third-party inventory beside them.

The image builds from this directory, so it cannot reach the repository's root LICENSE; tetriz-py keeps
its own copy. These tests keep that copy identical to the root, keep the Dockerfile copying the files into
the image, and keep THIRD_PARTY_LICENSES.md in step with every runtime and ``ml`` package uv.lock pins.
"""

from __future__ import annotations

import re
import tomllib
from pathlib import Path

PROJECT_DIR = Path(__file__).resolve().parent.parent
REPO_ROOT = PROJECT_DIR.parent
PROJECT_NAME = "tetriz-py"
LICENSE_DIR = "/usr/share/licenses/tetriz-py/"


def test_license_is_a_copy_of_the_root_license() -> None:
    assert (PROJECT_DIR / "LICENSE").read_bytes() == (REPO_ROOT / "LICENSE").read_bytes()


def test_notice_names_the_mit_license() -> None:
    notice = (PROJECT_DIR / "NOTICE").read_text()
    assert "Copyright 2026 Workloom Solutions Private Limited" in notice
    assert "MIT License" in notice


def test_dockerfile_copies_the_licence_files_into_the_image() -> None:
    dockerfile = (PROJECT_DIR / "Dockerfile").read_text()
    copies = [line.split()[1:] for line in dockerfile.splitlines() if line.startswith("COPY ")]
    shipped = {src for args in copies if args[-1] == LICENSE_DIR for src in args[:-1]}
    assert {"LICENSE", "NOTICE", "THIRD_PARTY_LICENSES.md"} <= shipped


def _lock_packages() -> dict[str, list[dict]]:
    lock = tomllib.loads((PROJECT_DIR / "uv.lock").read_text())
    by_name: dict[str, list[dict]] = {}
    for package in lock["package"]:
        by_name.setdefault(package["name"], []).append(package)
    return by_name


def _closure(by_name: dict[str, list[dict]], roots: list[dict]) -> set[tuple[str, str]]:
    """Every (name, version) reachable from roots, following each dependency's requested extras."""
    seen: set[tuple[str, str]] = set()
    stack = list(roots)
    while stack:
        dep = stack.pop()
        for package in by_name[dep["name"]]:
            if "version" in dep and package["version"] != dep["version"]:
                continue
            key = (package["name"], package["version"])
            extras = dep.get("extra", [])
            stack.extend(dep_ for extra in extras for dep_ in package.get("optional-dependencies", {}).get(extra, []))
            if key in seen:
                continue
            seen.add(key)
            stack.extend(package.get("dependencies", []))
    return seen


def _documented(section: str) -> set[tuple[str, str]]:
    text = (PROJECT_DIR / "THIRD_PARTY_LICENSES.md").read_text()
    body = text.split(f"## {section}\n", 1)[1].split("\n## ", 1)[0]
    rows = re.findall(r"^\| ([a-z0-9][a-z0-9._-]*) \| ([^ |]+) \|", body, flags=re.MULTILINE)
    return set(rows)


def test_third_party_inventory_matches_the_lock() -> None:
    by_name = _lock_packages()
    project = by_name[PROJECT_NAME][0]
    runtime = _closure(by_name, project["dependencies"]) - {(PROJECT_NAME, project["version"])}
    with_ml = _closure(by_name, project["dependencies"] + project["optional-dependencies"]["ml"])
    ml_only = with_ml - runtime - {(PROJECT_NAME, project["version"])}

    assert _documented("Runtime") == runtime
    assert _documented("Optional `ml` extra") == ml_only
