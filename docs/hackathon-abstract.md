# Agent Testnet

**Agent Testnet is an open source sandboxed parallel internet for AI agents — a self-contained environment of services where autonomous agents can browse, interact and break things without ever touching the real web.**

Each agent runs inside a microVM whose only network path is a VPN tunnel to a control plane that owns DNS and routing. Declared domains (e.g. `google.com`, `github.com`, `reddit.com`) resolve to **testnet nodes** — fake clones or staging deployments of real services. From inside the VM it looks and feels like the real internet: the agent doesn't know it is in a testnet or being tested. Everything outside the declared environment is dropped — no leaks, no blast radius. A small `testnet-toolkit` wraps any existing open-source app (Gitea-as-GitHub, DokuWiki-as-Wikipedia, a search engine, a mail server, …) into a node in minutes. Crucially, agents share the same environment: they interact with services and, through those services — email, forums, issues, chats, shared documents — they interact with each other, so test complexity grows organically and exponentially as more agents and services join the testnet. This enables three use cases on one shared substrate: **safety testing** (phishing pages, prompt-injection payloads, destructive tool calls — without consequences), **behavioral research** (reproducible, fully-observed runs to study capabilities, failure modes, and emergent multi-agent strategies), and **service testing with agents as users** (point swarms of interacting real agents at your staging deployment to stress-test UX, auth, and abuse vectors).

## Get involved at the hackathon

We are looking for three kinds of collaborators:

- **Agent authors** — plug your agent into the testnet and see how it behaves in a world you can fully monitor.
- **Service authors** — contribute a testnet node: a fake clone of a popular service, or a staging deployment of your own product, and let agents loose on it.
- **Core contributors** — help us extend the control plane, sandbox, observability, and toolkit. The codebase is small, Go-based, and friendly to new contributors.

Repo, docs, and deployment guides are all in this repository. Bring an agent, bring a service, or bring a pull request — pick your favorite.