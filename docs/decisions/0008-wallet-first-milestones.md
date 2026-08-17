# ADR-0008: Wallet history outranks code milestones

**Date:** 2026-08-16 · **Status:** accepted · **Source:** spec §1, §10

## Context

Job applications ask for the ENS name; a reviewer sees whatever the wallet holds *on the day they look*. Paper fills, sim-venue traffic, and Grafana leave no on-chain trace. A build that ships perfect code in week 8 with an empty wallet fails the project's first purpose.

## Decision

Every week ends with more real, explainable on-chain activity than it started with — manual carries from week 0b, executed by hand per the [playbook](../manual-carry-playbook.md), regardless of code progress. If a code milestone slips, the week's manual carry still happens; the wallet column of the milestone table never goes backwards. Priority 1 (wallet) outranks priorities 2–5 whenever they conflict. Corollary (wallet doctrine): activity stays small, real, readable, and exclusively carry-related — the wallet is a résumé line anyone can audit.

## Alternatives considered

- **Code first, trade when automated (wk 5+)** — five weeks of empty wallet during an active application window, and the automation would have no hand-validated reference sequence to reproduce.
- **Padding the wallet with unrelated activity** — rejected outright; illegible or degenerate history is worse than sparse history.

## Consequences

The build gains a weekly non-code obligation and a standing validation asset: manual-carry logs become parity baselines for the paper engine and backtester. Manual trading at small size costs real (bounded) money — accepted as the price of the artifact.
