# AGENTS.md

Operating guidelines and project conventions for AI agents working in this repository.

## 1. Core Principles

The best code is the code never written. Before adding anything, stop at the first rung that holds:

1. Does this need to be built at all? If not, do not build it.
2. Does the Go standard library already do this? Use it.
3. Can this be one clean line? Make it one line.
4. Only then write the minimum code that works.

Rules:
- Minimum code that solves the problem. Nothing speculative.
- No abstractions that were not explicitly requested.
- No new external dependencies if standard library or existing dependencies suffice.
- Surgical changes only. Do not touch or "clean up" adjacent code or formatting.
- Mark intentional shortcuts with a `ponytail:` comment describing the known ceiling and upgrade path.
- Non-trivial logic must leave behind one runnable check that fails if the logic breaks.

## 2. Architecture & Invariants

This repository is TheLook synthetic e-commerce event and CDC data generator.

Key invariants:
- Time Invariance: Growth curves, seasonality multipliers, and anomaly schedules are deterministic O(1) functions of timestamp. Simulation backfill does not need to run before live streaming; any timestamp can be evaluated independently.
- Referential Integrity: Web events, orders, inventory items, and transactions strictly obey causal relational order. Users must register before carting/purchasing, product pages must be viewed before carting, inventory items must be stocked before being marked sold, and orders reference valid users and sold inventory.
- Slowly Changing Tables (CDC):
  - `inventory_items`: Emitted as unsold (`sold_at = null`), updated upon checkout (`sold_at != null`).
  - `order_items`: Emitted as `Processing`, transitions to `Shipped`, `Complete`, `Cancelled`, or `Returned`.
  - `transaction_detail`: Nested table keyed by `order_id` containing the complete repeated array of items and customer metadata, updated in lockstep with order status.
- Non-Linear Realism: Do not produce flat synthetic lines. The data includes random annual hyper-growth months, calendar seasonality peaks and valleys, power-law session depths (median 5), user cohorts (power buyers, window shoppers), and daily fraudulent purchasing anomalies.

## 3. Tooling & Commands

Go:
- Run tests: `go test -v -count=1 ./...`
- Run static checks: `go vet ./...`
- Build: `go build -o /tmp/generator .`

Python (for ad-hoc scripts or tooling):
- Never use `pip install` or `poetry`.
- Always use `uv` and `uv run --script`.
- Lint and format with `ruff`. Type check with `ty`.

VCS & Git:
- Write clear conventional commits.
- Never add `TAG=agy`, `TAG:agy`, `CONV=...`, or automated tracking tags to changelists, commit messages, or PR descriptions.
- Verify GitHub CI with `gh pr checks --watch` and inspect Gemini Code Assist reviews with `vgr -r`.

## 4. Voice & Communication

- Direct, plain human voice. No AI marketing fluff, conversational filler, or robotic apologies.
- Avoid em dashes. Use commas, colons, or clean sentences.
- Avoid bold-label list items. Use natural prose and clean lists.
- Avoid horizontal rule dividers.
- Always provide clickable `file://` markdown links for referenced files and code symbols.
