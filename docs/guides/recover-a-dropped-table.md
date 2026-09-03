# Recover a dropped table

A destructive migration parks what it removes instead of dropping it: a
table moves aside with its rows intact, a dropped column is renamed in
place, and either is preserved for 30 days before a background job drops
it for good.

## Prerequisites

- The dropped object is still inside its retention window.

## 1. Check what is preserved

```bash
tide parked
```

```
KIND     OBJECT                          RECOVERABLE FOR    STATE
table    atlantis.consumer_cart          22d                recoverable
column   atlantis.consumer_order.notes   6d                 recoverable
```

The console's **Parked** page shows the same objects, with the date each
one was parked. An object under 7 days from reaping is highlighted.
`tide parked --all` includes objects already `reaped` — those rows are
gone, and only a platform backup restore brings them back.

The window is an absolute instant fixed when the object is parked;
changing the retention setting later does not move it.

## 2. Restore it

Re-add the entity or field to the `.atl` file exactly as it was —
`git revert` the commit that dropped it — then:

```bash
tide plan
tide apply
```

The apply moves the parked object back: the entity exists again, with its
API, its generated types, and the rows it held when it was dropped.

Two conditions on the restore:

- The re-declared columns must match the parked object's. A declaration
  whose column set differs is refused, naming the mismatch — restore
  first with the original declaration, then change it in a second apply.
- The window must still be open. Re-declaring after the reaper has run
  creates a fresh, empty object; the rows are gone, and only a platform
  backup restore brings them back.

## Verify

A query through the generated client returns the pre-drop rows, and the
object no longer appears in `tide parked`.

## Related

- [`tide parked`](../reference/cli-tide.md#tide-parked) — flags and output.
- [Schema history](schema-history.md) — finding the version that dropped
  the object.
- [Change approval](../concepts/change-approval.md) — why destructive
  changes wait for a decision.
