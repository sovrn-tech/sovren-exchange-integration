# Contacts & Service Points (FR-053)

Published, resolvable contact and service points for exchanges integrating
SOVR. Operating and staffing these channels is a Sovren operational commitment.

> **Release gate**: every entry in this file is verified *resolvable* at release
> time (FR-053) — the security contact must also appear in `SECURITY.md`, the
> status page must carry a URL, and every URL must return 2xx/3xx. That check is
> implemented in two places: **export stage 3** (fails a real export if any value
> is missing or unresolvable, and records a `contacts` documented gap while any
> FR-053 value is still an unpublished placeholder — a GA cut fails on it) and
> **certification scenario G1** (BLOCKED(D5) until published, then PASS/FAIL).

## Technical contacts

Sovren operates a single shared technical support point (email + Discord); the
primary and secondary rows below intentionally reference it.

| Role | Contact | Expectation |
|------|---------|-------------|
| Primary technical contact | support@sovrentech.io · [Discord support channel](https://discord.com/channels/1496907756718915645/1496907758283526168) | integration questions, certification support; business-hours response |
| Secondary technical contact | support@sovrentech.io · [Discord support channel](https://discord.com/channels/1496907756718915645/1496907758283526168) (shared with primary) | fallback when primary is unavailable |

## Security contact

| Role | Contact | Expectation |
|------|---------|-------------|
| Security reports | support@sovrentech.io (see also `SECURITY.md`) | vulnerability disclosure + suspected-compromise reports (incident class 11); acknowledged per the SECURITY.md SLA |

## Channels

| Channel | Location | Purpose |
|---------|----------|---------|
| Upgrade notifications | [Discord upgrades & incidents channel](https://discord.com/channels/1496907756718915645/1496907757897383974) | advance notice of chain upgrades (pair with the `UpgradeHeightApproaching` alert and `docs/upgrades.md`) |
| Emergency incident channel | [Discord upgrades & incidents channel](https://discord.com/channels/1496907756718915645/1496907757897383974) (currently shared with upgrade notifications) | chain halts, security incidents, coordinated pauses (docs/incident-response.md) |

## Status page

| Service | Location | Purpose |
|---------|----------|---------|
| Network status page | https://sovrscan.com/node-health (uptime monitoring per plan D11) | public endpoint + chain health; first stop during incident classes 2–4 |

## What exchanges should wire up now

1. Route the kit's alert pack (`monitoring/alerts/`) into your own paging system
   — the adapter never depends on Sovren channels to detect problems.
2. Subscribe your ops rotation to the upgrade-notification channel above.
3. Record your own escalation path in your runbook next to each incident class
   in `docs/incident-response.md`.
