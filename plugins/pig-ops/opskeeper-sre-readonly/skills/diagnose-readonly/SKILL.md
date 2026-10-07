---
name: diagnose-readonly
description: >-
  Gather read-only diagnostic evidence from a node and its surrounding
  infrastructure. Use when investigating an incident, triaging an alert,
  or establishing current state before proposing any change. This skill
  never mutates anything.
---

# Read-only diagnosis

Establish what is actually true before anyone proposes a change. Every tool
this skill reaches for is read-only, so the whole procedure is safe to run
against production without an approval round trip.

## When to use this skill

- An alert fired and the cause is not yet known.
- An incident needs evidence gathered before a repair can be proposed.
- A human asked "what is going on with this service right now".

Prefer this over guessing. A repair proposed without evidence is a guess
with a blast radius.

## Procedure

1. **Scope the blast radius.** Identify which node, namespace, or
   dependency the alert concerns. Narrow the search before widening it.

2. **Collect primary evidence.** Read the service's own logs, its process
   state, and its resource usage. Prefer the service's own account of
   itself over an inferred one.

3. **Correlate with topology.** A slow database is a different incident
   from a slow application. Use the topology graph to find what the
   affected component actually talks to.

4. **Check recent change.** Look for a deploy, a config change, or a
   scaling event in the window around the alert. Most incidents trace back
   to a change.

5. **State a hypothesis and its confidence.** Say what you believe is
   wrong, what evidence supports it, and what would falsify it. An
   unquantified hypothesis is not a finding.

## What this skill will not do

This skill does not restart services, change configuration, or apply
manifests. Those are mutating operations and ship in separately approved
packages. If your investigation concludes that a change is needed, say so
and hand off — do not perform the change from here.

## Reporting

Report findings in this order:

- What is broken, stated in one sentence.
- The evidence, with where it came from.
- The most likely cause and your confidence in it.
- What you ruled out.
- The smallest change that would fix it, if you were asked to.
