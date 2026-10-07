# Pi Vertical Plugin Workspace

This directory hosts independently versioned Pi plugins developed by the OPC
squad. Pient is the runtime base; OPC is the control plane.

Required layout:

```text
<plugin-id>/
  package.json
  pient-plugin.json
  LICENSE
  README.md
  extensions/
  skills/
  src/
  tests/
```

See `docs/ARCHITECTURE_GUARDRAILS.md` and run `make pi-plugin-guard` before
review. The workspace may currently be empty; once a directory declares itself
as a package, the architecture guard treats it as a release candidate.
