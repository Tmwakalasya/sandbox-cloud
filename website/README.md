# Sandbox Cloud landing page

A standalone, dependency-free static site. Buildkite-inspired visual direction,
original layout, copy, and graphics. No tracking, external fonts, image downloads,
API connection, or build step.

From the repository root:

```sh
make site        # http://127.0.0.1:3000
make site-check  # Node.js 18+; no npm install
```

Serve this directory from any static host if desired. Nothing is published by the
development commands. No Kubernetes or credentials are needed. The workspace
preview is explicitly simulated; the page never executes shell commands.

Interactions: responsive navigation, keyboard-accessible setup tabs, command
copying with a selectable-text fallback, replayable execution demo, and native
FAQ disclosures. Reduced-motion preferences are respected.
