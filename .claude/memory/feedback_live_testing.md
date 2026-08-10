---
name: live-testing-before-merge
description: Always live test installer changes on the actual machine before merging PRs
type: feedback
---

Always live test installer changes (hooks, config files, CLI detection) on an actual machine before merging. Don't rely solely on unit tests. Run the installer functions directly and verify the output files.

**Why:** Gemini installer support was merged without live testing and regressed. Codex was live-tested before merge and that caught a version parsing bug. Same rigor should apply to all installers.

**How to apply:** For any PR that writes config files (`hooks.json`, `settings.json`, `config.toml`, `.claude.json`, `plugin/repowire.ts`), build the native binary, run `repowire setup`, and inspect the output files before merging.
