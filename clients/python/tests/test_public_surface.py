"""The surface generated code imports.

Every Python tree ``tide generate`` emits opens with these imports, so a rename
here breaks every caller's build rather than this package's. The module paths
are part of the contract: the emitter writes ``from atlantis_client import
errors as _errors``, so ``errors`` must stay a module and not become a name
re-exported from somewhere else.

Nothing here reads a path relative to this file, so the same file runs against
an installed wheel.
"""

from __future__ import annotations

import importlib
import inspect

CONTRACT: dict[str, tuple[str, ...]] = {
    "atlantis_client.errors": ("translate", "UnsupportedQueryOption"),
    "atlantis_client.pagination": ("iterate_pages", "aiterate_pages"),
    "atlantis_client.tenant": ("metadata_for",),
    "atlantis_client.jobs": ("Registry",),
    "atlantis_client.jobs.argjson": ("IntervalJSON",),
}

# A contract that lost its entries asserts nothing while still passing.
MIN_NAMES = 7


def test_the_contract_is_not_empty() -> None:
    assert sum(len(names) for names in CONTRACT.values()) >= MIN_NAMES


def test_every_module_resolves() -> None:
    for path in CONTRACT:
        importlib.import_module(path)


def test_every_name_resolves() -> None:
    for path, names in CONTRACT.items():
        module = importlib.import_module(path)
        for name in names:
            assert hasattr(module, name), f"{path}.{name} is gone"


def test_register_takes_two_positional_arguments() -> None:
    """The emitter calls it positionally.

    Keyword-only parameters would break every generated jobs file while every
    import above still resolved.
    """
    registry = importlib.import_module("atlantis_client.jobs").Registry
    params = list(inspect.signature(registry.register).parameters.values())[1:]
    assert len(params) == 2
    for param in params:
        assert param.kind is inspect.Parameter.POSITIONAL_OR_KEYWORD
