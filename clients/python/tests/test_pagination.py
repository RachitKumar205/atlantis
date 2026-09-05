"""Walking pages, and the short-page trap."""

from __future__ import annotations

import asyncio
from dataclasses import dataclass, field

import pytest

from atlantis_client.pagination import SERVER_MAX_LIMIT, aiterate_pages, iterate_pages


@dataclass
class Page:
    entities: list[int] = field(default_factory=list)
    next_page_token: str = ""


def test_walks_until_the_token_is_empty() -> None:
    pages = {
        "": Page([1, 2], "p2"),
        "p2": Page([3, 4], "p3"),
        "p3": Page([5], ""),
    }
    got = list(iterate_pages(lambda tok: pages[tok], lambda p: p.entities))
    assert got == [1, 2, 3, 4, 5]


def test_a_short_page_does_not_end_the_walk() -> None:
    """The server clamps limit to 1000, so a full result set arrives short.

    Stopping on `len(rows) < limit` returns the first page and reports success.
    A caller asking for 5000 would silently receive 1000 of them.
    """
    pages = {
        "": Page(list(range(SERVER_MAX_LIMIT)), "p2"),
        "p2": Page([9999], ""),
    }
    got = list(iterate_pages(lambda tok: pages[tok], lambda p: p.entities))
    assert len(got) == SERVER_MAX_LIMIT + 1
    assert got[-1] == 9999


def test_an_empty_page_carrying_a_token_still_continues() -> None:
    """A filter can exclude every row of a page while later pages hold rows."""
    pages = {
        "": Page([], "p2"),
        "p2": Page([7], ""),
    }
    assert list(iterate_pages(lambda tok: pages[tok], lambda p: p.entities)) == [7]


def test_a_repeated_token_raises_rather_than_looping() -> None:
    """A server repeating a token would otherwise hang the caller for ever."""
    with pytest.raises(RuntimeError, match="already issued"):
        list(iterate_pages(lambda _tok: Page([1], "same"), lambda p: p.entities))


def test_fetch_receives_the_previous_token() -> None:
    seen: list[str] = []

    def fetch(token: str) -> Page:
        seen.append(token)
        return Page([len(seen)], "next" if len(seen) < 2 else "")

    list(iterate_pages(fetch, lambda p: p.entities))
    assert seen == ["", "next"]


def test_async_walk_matches_the_sync_one() -> None:
    pages = {
        "": Page([1, 2], "p2"),
        "p2": Page([3], ""),
    }

    async def fetch(token: str) -> Page:
        return pages[token]

    async def run() -> list[int]:
        return [row async for row in aiterate_pages(fetch, lambda p: p.entities)]

    assert asyncio.run(run()) == [1, 2, 3]


def test_async_repeated_token_raises() -> None:
    async def fetch(_token: str) -> Page:
        return Page([1], "same")

    async def run() -> list[int]:
        return [row async for row in aiterate_pages(fetch, lambda p: p.entities)]

    with pytest.raises(RuntimeError, match="already issued"):
        asyncio.run(run())
