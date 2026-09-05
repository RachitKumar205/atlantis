"""Walking a Query's pages.

A page ends the walk only when ``next_page_token`` comes back empty.

A short page does not end it. The server clamps ``limit`` to 1000, so a request
for 5000 returns 1000 rows with more still to read; stopping on
``len(rows) < limit`` would silently return a fifth of the result set. The
token is the only signal the server gives, and it gives it exactly.
"""

from __future__ import annotations

from typing import TYPE_CHECKING, Protocol, TypeVar

if TYPE_CHECKING:
    from collections.abc import AsyncIterator, Awaitable, Callable, Iterator, Sequence

__all__ = ["SERVER_MAX_LIMIT", "aiterate_pages", "iterate_pages"]

# What the server clamps `limit` to. Mirrored here so a client can explain a
# short page, not to pre-clamp: a request for more is not an error and the
# server's own value is the one that decides.
SERVER_MAX_LIMIT = 1000

Row = TypeVar("Row")


class _Page(Protocol):
    """The shape every Query response shares."""

    next_page_token: str


def iterate_pages(
    fetch: Callable[[str], _Page],
    rows_of: Callable[[_Page], Sequence[Row]],
) -> Iterator[Row]:
    """Yield every row across every page.

    ``fetch`` takes a page token — empty for the first page — and returns one
    response. Each response's token is passed to the next call.

    A token the server repeats would loop for ever, so a repeat ends the walk:
    that is a server defect, and hanging is a worse way to report it than
    stopping is.
    """
    token = ""
    seen: set[str] = set()
    while True:
        page = fetch(token)
        yield from rows_of(page)
        token = page.next_page_token
        if not token:
            return
        if token in seen:
            raise RuntimeError(
                "the server returned a page token it had already issued, so this "
                "walk would not terminate. Report the entity and the filter."
            )
        seen.add(token)


async def aiterate_pages(
    fetch: Callable[[str], Awaitable[_Page]],
    rows_of: Callable[[_Page], Sequence[Row]],
) -> AsyncIterator[Row]:
    """:func:`iterate_pages` over an async ``fetch``."""
    token = ""
    seen: set[str] = set()
    while True:
        page = await fetch(token)
        for row in rows_of(page):
            yield row
        token = page.next_page_token
        if not token:
            return
        if token in seen:
            raise RuntimeError(
                "the server returned a page token it had already issued, so this "
                "walk would not terminate. Report the entity and the filter."
            )
        seen.add(token)
