---
title: OpsKeeper · Agent 原生运维工作台
emoji: 🛡️
colorFrom: blue
colorTo: indigo
sdk: gradio
sdk_version: 5.49.1
app_file: app.py
pinned: false
license: Apache License 2.0
---

# OpsKeeper ModelScope Creative Space

This read-only Gradio showcase presents the OpsKeeper product journey, architecture, safety boundary, and public-safe evidence assets. The app looks for its local Markdown and evidence assets in the repository layout created by ModelScope and does not connect to Manager, PostgreSQL, Matrix, AgentTeams, or any server-side write API.

## Safety claims

preview-pg + controlled fixed-load replay; passing preview only yields HITL eligibility; no PolarDB HA claim; no active-session copying claim.

## Run locally

```bash
python -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
python app.py
```

Open `http://127.0.0.1:7860`. External entry cards open in a new window with `rel="noopener noreferrer"` and keep authentication in the original service. Do not add secrets, private endpoints, privileged clients, or write APIs to this showcase.
