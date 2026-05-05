---
title: Daili Usage Keeper
emoji: 📊
colorFrom: indigo
colorTo: blue
sdk: docker
app_port: 8080
pinned: false
---

# Daili Usage Keeper

A standalone [CPA Usage Keeper](https://github.com/Willxup/cpa-usage-keeper) Space for `pjpjq/daili`.

This Space intentionally runs separately from the main CPA Space so the proxy path is not coupled to usage visualization.

Required secret before it starts the real dashboard:

- `CPA_MANAGEMENT_KEY`: management key for `https://pjpjq-daili.hf.space`

Default variables:

- `CPA_BASE_URL=https://pjpjq-daili.hf.space`
- `REDIS_QUEUE_ADDR=127.0.0.1:9` to force fast HTTP fallback to CPA `/v0/management/usage-queue`, because Hugging Face Spaces cannot reach the raw CPA TCP/RESP port across Spaces.
- `AUTH_ENABLED=false` because this Space is intended to be private. Enable it and set `LOGIN_PASSWORD` if the Space is made public.

Persistent history requires Hugging Face persistent storage or another backup strategy for `/data`.
