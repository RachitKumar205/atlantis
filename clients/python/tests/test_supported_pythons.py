"""One list of interpreters, spelled in three places.

`requires-python`, the `Programming Language :: Python :: 3.x` classifiers and
the CI matrix each say which interpreters this package runs on, and a release
claims all three to whoever installs it. Held together here because a classifier
is metadata nobody executes: a version added to the matrix and not to the
classifiers is tested and undeclared, and one added to the classifiers and not
to the matrix is declared and untested.
"""

from __future__ import annotations

import re
import sys
from pathlib import Path
from typing import Any

if sys.version_info >= (3, 11):
    import tomllib
else:
    import tomli as tomllib

REPO = Path(__file__).resolve().parents[3]
PYPROJECT = REPO / "clients" / "python" / "pyproject.toml"
WORKFLOW = REPO / ".github" / "workflows" / "ci.yml"

CLASSIFIER = re.compile(r"^Programming Language :: Python :: (3\.\d+)$")
MATRIX = re.compile(r"^\s*python-version:\s*\[([^\]]+)\]\s*$", re.MULTILINE)

# A parse that reads nothing leaves every comparison below over empty sets.
MIN_VERSIONS = 2


def project() -> dict[str, Any]:
    with PYPROJECT.open("rb") as fh:
        parsed: dict[str, Any] = tomllib.load(fh)
    assert parsed["project"]["name"] == "atlantis-client"
    return parsed


def declared() -> set[str]:
    found = set()
    for classifier in project()["project"]["classifiers"]:
        match = CLASSIFIER.match(classifier)
        if match:
            found.add(match.group(1))
    return found


def matrix_versions() -> set[str]:
    match = MATRIX.search(WORKFLOW.read_text())
    assert match, f"no python-version matrix in {WORKFLOW}"
    return {value.strip().strip('"').strip("'") for value in match.group(1).split(",")}


def test_the_lists_are_not_empty() -> None:
    assert len(declared()) >= MIN_VERSIONS
    assert len(matrix_versions()) >= MIN_VERSIONS


def test_every_declared_version_is_tested() -> None:
    assert declared() == matrix_versions()


def test_the_floor_is_the_lowest_tested_version() -> None:
    lowest = min(matrix_versions(), key=lambda v: tuple(int(part) for part in v.split(".")))
    assert project()["project"]["requires-python"] == f">={lowest}"
