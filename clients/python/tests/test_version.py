"""One version string, not two.

The version lives in ``atlantis_client.__version__``; ``pyproject.toml``
declares it dynamic and reads it from there. With one string there is nothing
left to compare, so what is asserted here is the mechanism: a literal version
back in ``pyproject.toml`` is what these tests refuse.

The value check — that a built artefact carries the same string — belongs
against an installed wheel, where it is not vacuous.
"""

from __future__ import annotations

import sys
from pathlib import Path
from typing import Any

if sys.version_info >= (3, 11):
    import tomllib
else:
    import tomli as tomllib

import atlantis_client

PYPROJECT = Path(__file__).resolve().parents[1] / "pyproject.toml"


def project() -> dict[str, Any]:
    """The parsed file, with the name asserted.

    A misdirected read parses to an empty mapping and every assertion below
    passes over it.
    """
    with PYPROJECT.open("rb") as fh:
        parsed: dict[str, Any] = tomllib.load(fh)
    assert parsed["project"]["name"] == "atlantis-client"
    return parsed


def test_pyproject_carries_no_static_version() -> None:
    assert "version" not in project()["project"]


def test_the_version_is_declared_dynamic() -> None:
    assert "version" in project()["project"].get("dynamic", [])


def test_the_build_reads_the_module_that_is_imported() -> None:
    """The configured path and the imported package are the same file."""
    configured = PYPROJECT.parent / project()["tool"]["hatch"]["version"]["path"]
    assert configured.is_file()
    assert Path(atlantis_client.__file__).resolve() == configured.resolve()


def test_the_module_defines_a_version() -> None:
    assert atlantis_client.__version__
