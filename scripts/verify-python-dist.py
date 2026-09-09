"""Check the two files a Python release uploads, and what the wheel installs to.

    python -I scripts/verify-python-dist.py --wheel W --sdist S \
        --gitignore clients/python/.gitignore [--require-installed --version V]

The archive checks run anywhere. `--require-installed` adds the checks that
need the wheel unpacked into the environment running this, which is why
`make release-python-verify` runs it from a throwaway environment outside the
repository under `python -I`.

Three things an archive can lose or gain without failing to build:

  py.typed        mypy then reports `import-untyped` at every caller, and a
                  caller who has set ignore_missing_imports sees nothing at all
  _wire/*.pyi     the same, for the generated protobuf modules
  a stray file    the backend force-adds the nearest .gitignore walking up to
                  the repository root, and the source distribution takes the
                  working tree rather than the index
"""

from __future__ import annotations

import argparse
import pathlib
import re
import sys
import tarfile
import zipfile

WHEEL_MEMBERS = (
    "atlantis_client/py.typed",
    "atlantis_client/_wire/frame_pb2.pyi",
)

SDIST_PREFIX = "src/atlantis_client/"
SDIST_FILES = frozenset({"PKG-INFO", "pyproject.toml", "README.md", "LICENSE", ".gitignore"})

WHEEL_NAME = re.compile(r"^atlantis_client-(?P<version>[^-]+)-py3-none-any\.whl$")
SDIST_NAME = re.compile(r"^atlantis_client-(?P<version>.+)\.tar\.gz$")


def check_wheel(wheel: pathlib.Path, problems: list[str]) -> None:
    names = set(zipfile.ZipFile(wheel).namelist())
    for member in WHEEL_MEMBERS:
        if member not in names:
            problems.append(f"{wheel.name} does not contain {member}")
    if not any(name.endswith(".dist-info/licenses/LICENSE") for name in names):
        problems.append(f"{wheel.name} carries no licence file")


def check_sdist(sdist: pathlib.Path, gitignore: pathlib.Path, problems: list[str]) -> None:
    with tarfile.open(sdist) as archive:
        root = f"{sdist.name[: -len('.tar.gz')]}/"
        members = [m for m in archive.getmembers() if m.isfile()]
        for member in members:
            name = member.name[len(root) :] if member.name.startswith(root) else member.name
            if name.startswith(SDIST_PREFIX) or name in SDIST_FILES:
                continue
            problems.append(f"{sdist.name} contains {name}, which no release should publish")

        shipped = archive.extractfile(f"{root}.gitignore")
        if shipped is None:
            problems.append(f"{sdist.name} contains no .gitignore, so the backend found none")
        elif shipped.read() != gitignore.read_bytes():
            problems.append(
                f"{sdist.name} ships a .gitignore that is not {gitignore} — the backend "
                f"walked past it to the repository root"
            )


def check_installed(version: str, problems: list[str]) -> None:
    import atlantis_client

    installed = pathlib.Path(atlantis_client.__file__).resolve()
    prefix = pathlib.Path(sys.prefix).resolve()
    if not installed.is_relative_to(prefix):
        problems.append(f"imported {installed}, which is outside the environment at {prefix}")
    if atlantis_client.__version__ != version:
        problems.append(
            f"the artefacts are named {version} and the installed module reports "
            f"{atlantis_client.__version__}"
        )


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--wheel", type=pathlib.Path, required=True)
    parser.add_argument("--sdist", type=pathlib.Path, required=True)
    parser.add_argument("--gitignore", type=pathlib.Path, required=True)
    parser.add_argument("--require-installed", action="store_true")
    parser.add_argument("--version")
    args = parser.parse_args()

    problems: list[str] = []

    wheel_match = WHEEL_NAME.match(args.wheel.name)
    sdist_match = SDIST_NAME.match(args.sdist.name)
    if wheel_match is None:
        problems.append(f"{args.wheel.name} is not a wheel this project builds")
    if sdist_match is None:
        problems.append(f"{args.sdist.name} is not a source distribution this project builds")
    if wheel_match is None or sdist_match is None:
        print("\n".join(problems), file=sys.stderr)
        return 1

    version = wheel_match["version"]
    if sdist_match["version"] != version:
        problems.append(f"the wheel says {version} and the source distribution says {sdist_match['version']}")
    if args.version is not None and args.version != version:
        problems.append(f"asked for {args.version} and the artefacts are named {version}")

    check_wheel(args.wheel, problems)
    check_sdist(args.sdist, args.gitignore, problems)
    if args.require_installed:
        check_installed(version, problems)

    for problem in problems:
        print(problem, file=sys.stderr)
    if problems:
        return 1
    print(f"atlantis-client {version}: both artefacts carry only what a release should")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
