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

## 2. Restore the declaration

Re-add the entity or field to the `.atl` file as it was — `git revert` the
commit that dropped it — then:

```bash
tide plan
tide apply
```

This restores the schema: the entity exists again, with its API and its
generated types. It creates a fresh, empty object; the preserved rows stay
parked.

## 3. Restore the rows

Contact atlantis support with the object's name from `tide parked` while
its window is open, so the preserved rows can be moved back into the
restored object.

## Verify

`tide show <path-substring>` — matched against the `.atl` file's path —
prints the submitted file with the restored declaration in it. After the
row restore, a query returns the pre-drop data.

## Related

- [`tide parked`](../reference/cli-tide.md#tide-parked) — flags and output.
- [Schema history](schema-history.md) — finding the version that dropped
  the object.
- [Change approval](../concepts/change-approval.md) — why destructive
  changes wait for a decision.
