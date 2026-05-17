# Personas

Each subdirectory here is a self-contained OpenClaw persona that can be
applied to an agent VM via:

```bash
bash deploy/aws-deploy.sh openclaw install   --client N --persona NAME ...
bash deploy/aws-deploy.sh openclaw reconfig  --client N --persona NAME --persona-confirm ...
```

When applied, the persona's files are uploaded to
`~/.openclaw/workspace/` on the agent VM (replacing any existing copies
of those specific files) and any `heartbeat.json` is spliced into
`agents.defaults.heartbeat` inside `~/.openclaw/openclaw.json`. The
gateway is restarted so the new identity and heartbeat take effect.

## Layout

Every persona directory may contain:

| File           | Purpose                                                            |
| -------------- | ------------------------------------------------------------------ |
| `IDENTITY.md`  | Name, creature, vibe, emoji — OpenClaw injects this at boot.       |
| `SOUL.md`      | Core motivation and in-character constraints.                      |
| `AGENTS.md`    | Operational rules: tools, accounts, workflow loop.                 |
| `USER.md`      | Who the human on the other end of the chat is.                     |
| `HEARTBEAT.md` | Checklist the agent runs each heartbeat tick.                      |
| `heartbeat.json` | Inline JSON for `agents.defaults.heartbeat` (interval, prompt). |

`apply_persona` in [scripts/install-openclaw.sh](../../scripts/install-openclaw.sh)
overwrites only these five `*.md` files and the heartbeat block. It
deletes `BOOTSTRAP.md` if present (so the persona ritual doesn't run)
and leaves `memory/`, `MEMORY.md`, and anything else in the workspace
untouched. On `reconfig` the helper prints a warning and requires an
interactive `y` confirmation or `OPENCLAW_PERSONA_CONFIRM=1` (the
deploy wrapper sets this when `--persona-confirm` is passed) before
overwriting.

`IDENTITY.md`, `AGENTS.md`, and `SOUL.md` are required; the rest are
optional.

## Current personas

| Name      | Role                                | Email                  |
| --------- | ----------------------------------- | ---------------------- |
| `lobby`   | Friendly naive lobster support bot. | `lobby@gmail.com`      |
| `mrsmith` | Adversarial agent attacking Lobby.  | `mrsmith@gmail.com`    |
| `eve`     | Generic adversarial red-team agent. | (no fixed mailbox)     |

`lobby` and `mrsmith` both run a 2-minute heartbeat driving the testnet
mail node ([docs/design_documents/mail-server-design.md](../../docs/design_documents/mail-server-design.md)).
`eve` ships with heartbeats disabled — she only acts when prompted.

## Adding a new persona

1. Create `configs/personas/<NAME>/` with the files above.
2. Required files: `IDENTITY.md`, `SOUL.md`, `AGENTS.md`.
3. Optional `heartbeat.json` if the persona should run on a schedule.
4. Verify on a VM with
   `bash deploy/aws-deploy.sh openclaw install --persona <NAME> ...`.
