# Approve a schema change

By default a change that breaks another caller or destroys data does not apply on its own — it waits for a person. This is that loop, from writing the change to seeing it applied.

Read [change approval](../concepts/change-approval.md) first if you want the model. This page is the recipe.

## 1. Find out the rule before you write the change

`tide plan` reports the rule for the class it produced:

```
$ tide plan
plan ───────────────────
  plan_id  9f2c1ab4e07d
  class    destructive
  approval required (admin)
```

The server decides this and says so. `tide` never works it out from the class, because the rule is a row in a table only the server reads — a CLI that guessed would print one answer while the apply enforced another.

No `approval` line means that class applies unattended on this deployment.

## 2. Apply, and expect to be held

```
$ tide apply
tide apply: admin: this change is destructive and needs approval from admin
before it can apply. Plan 9f2c1ab4e07d: recorded and waiting for a decision.

      Re-run once it has been decided, or pass --wait-for-approval=30m
      to hold this process open until then.
$ echo $?
2
```

`apply` reads its endpoint from the credential store, the same as `plan`.

**Exit code 2 is not a failure.** The gate did its job. The plan — the proposed `.atl` source, the diff, and the SQL that would run — is stored and waiting.

If this is CI, treat 2 as "waiting", not "broken".

## 3. Decide it in the console

Open **Approvals** in the console. Each row names the class, the caller, who requested it, which role has to decide, and when it expires.

Open one and you see the proposed `.atl` source **first**, and the emitted SQL second. That order is the point of the page. You are approving schema; the SQL is what the emitter made of it, and signing off on `ALTER TABLE … DROP COLUMN` without sight of the declaration that produced it is a signature, not a review.

Both buttons are disabled unless you hold the role the class names — the page tells you which one when you hover.

- **Approve** re-prompts for your password. It sits behind the same elevation as signing out every session and revoking every caller, because it is the button that lets production DDL run.
- **Reject** does not re-prompt, and requires a reason. The reason reaches the caller in the error their next apply returns, so write it for them.

The reason field is recorded either way. It is only mandatory on rejection.

Rejection is final. Resubmitting the same change does not re-open the request — change the schema and re-plan.

## 4. Apply again

```
$ tide apply
✔ applied at 2026-08-15T14:22:07Z
```

The approval is checked against what would run *now*, not against what was proposed. Both the file contents and the SQL the server would emit must match what was approved — comparing only the shape of the change would approve a diff and run a script. If the schema moved underneath the approval in a way that changes either, the apply is refused with an instruction to re-plan and ask again.

### Not wanting to re-run

`--wait-for-approval=30m` holds the process open and applies as soon as somebody decides.

It is not the default because `tide apply` runs on CI runners, and a polling default holds a runner for as long as review takes. Use it when a person is waiting at a terminal.

## 5. Change the policy

Console → **Settings** → **Change policy**. Each class has its own rule and its own approver role.

Two things worth knowing before you loosen one:

- **Deleting a row does not turn the gate off.** A class with no rule requires approval. So does a class whose name nobody recognises. Every way of failing to find a rule fails the same way — toward asking a human.
- **Every edit is attributed.** The table records who changed the rule and when, because a change to who may authorise production DDL is itself something you audit.

## Related

- [Change approval](../concepts/change-approval.md) — the defaults, and why they differ between fresh and upgraded installs.
- [Deploy to production](deploy-to-production.md) — the rest of the production checklist.
