---
description: >-
  Author an OpsKeeper alert rule through the draft → confirm → apply flow.
  Use when the user asks to create an alert rule (changing or removing one
  is console-only today; the command says so instead of improvising). This
  command contributes prompt discipline
  only: it wires no tools and grants no authority, and every mutating step
  still goes through draft_config_change and apply_config_change with an
  exact draft_hash.
allowed-tools:
  - draft_config_change
  - apply_config_change
  - query_alert_rules
  - list_metric_catalog
---

# Alert rule authoring

A rule is not a paragraph. It is a row in `alert_rules`, and the only
thing that makes a rule real is a `config_draft` payload plus the
`draft_hash` that was issued with it. Everything else you could write —
a nicely formatted rule summary, a proposed threshold, a "here is your
rule" reply — is prose the console will render next to a real draft with
nothing to tell them apart. Do not produce that prose.

## The flow, in order

1. **Read before you write.** Call `query_alert_rules` first when the user
   names an existing rule, and `list_metric_catalog` before drafting a
   metric rule unless the user supplied exact PromQL or an exact metric
   name. A rule drafted against an imagined metric fails validation; a
   rule drafted against a real metric the user did not mean fails at 3am.
2. **Draft.** One call to `draft_config_change`. It validates the
   candidate, previews it against real history, and returns either
   `config_validation_failed` (fix the issues and draft again) or
   `config_draft` with a `draft_hash`. If it returns validation issues,
   fix them and re-draft — do not hand the user the failed candidate as if
   it were a draft.
3. **Disclose the scope, then stop.** Read `scope.label` and
   `scope.type` out of the draft and say them out loud: a `host` rule
   pages when one machine breaks, a `global` rule pages when the service
   does. Also say what the rollback is — the draft carries it, and it
   names the rule it applies to. Then stop calling tools and wait.
4. **Apply only what was confirmed.** `apply_config_change` takes the
   unchanged `payload` and `draft_hash`. Never invent a hash, never
   re-draft silently after the user confirmed, and never apply after the
   user asked for a scope change — re-draft and disclose again.

## Actions

`action` is `create` today. `update` and `delete` are planned but are not
wired: the host's draft domain refuses any other action, so a call with
`action=update` fails validation rather than half-working. If the user asks
to change or remove a rule, say that the conversational path creates rules
only and the change has to be made in the console — do not simulate an
update by drafting a near-duplicate rule. When `update` arrives, enabling
or disabling a rule will be an `update` with `enabled` set, and for
`delete` the draft must carry the rule's current definition back to you:
that is the rollback, and it is the only copy that will exist after the
apply.

## Never

- Never emit a rule as text without a `draft_hash` to anchor it.
- Never apply on the same turn the user first asked for a rule.
- Never widen a scope the user asked for narrowly ("this host" → global)
  without saying so and re-drafting.
- Never treat a notification channel as part of this flow: naming
  `notify_channel_ids` is configuration, sending is not yours to do.
