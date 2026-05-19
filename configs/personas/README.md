# Personas

Each subdirectory here is a self-contained OpenClaw persona that can be
applied to an agent VM via:

```bash
bash deploy/aws-deploy.sh openclaw install   --client N --persona NAME ...
bash deploy/aws-deploy.sh openclaw reconfig  --client N --persona NAME --persona-confirm ...
```

When applied, the persona's `*.md` files are uploaded to
`~/.openclaw/workspace/` on the agent VM (replacing any existing copies
of those specific files) and the persona's `cron.json` jobs are
provisioned via `openclaw cron add` (idempotent by job `name`). The
gateway is restarted so the new identity takes effect.

## Layout

Every persona directory may contain:

| File           | Purpose                                                            |
| -------------- | ------------------------------------------------------------------ |
| `IDENTITY.md`  | Name, creature, vibe, emoji — OpenClaw injects this at boot.       |
| `SOUL.md`      | Core motivation and in-character constraints.                      |
| `AGENTS.md`    | Operational rules: tools, accounts, workflow loop.                 |
| `USER.md`      | Who the human on the other end of the chat is.                     |
| `HEARTBEAT.md` | Checklist the agent runs each scheduled tick (read by the cron payload). |
| `cron.json`    | Cron jobs to create on install (interval, payload, session). |

`apply_persona` in [scripts/install-openclaw.sh](../../scripts/install-openclaw.sh)
overwrites only these five `*.md` files and re-applies the named cron
jobs. It deletes `BOOTSTRAP.md` if present (so the persona ritual
doesn't run) and leaves `memory/`, `MEMORY.md`, and anything else in
the workspace untouched. On `reconfig` the helper prints a warning and
requires an interactive `y` confirmation or `OPENCLAW_PERSONA_CONFIRM=1`
(the deploy wrapper sets this when `--persona-confirm` is passed) before
overwriting.

`IDENTITY.md`, `AGENTS.md`, and `SOUL.md` are required; the rest are
optional.

### `cron.json` schema

```json5
{
  "jobs": [
    {
      "name": "support-tick",          // required, unique within the persona
      "every": "2m",                   // OR "cron": "*/5 * * * *", OR "at": "+10m"
      "session": "main",               // "main" | "isolated"; default "main"
      "wake": "now",                   // "now" | "next-heartbeat"; default "now"
      "systemEvent": "Read HEARTBEAT.md ...",   // user message injected into the agent
      // -- all optional --
      "message": "...",                // alternative to systemEvent (one of the two)
      "tz": "UTC",                     // for --cron expressions
      "model": "anthropic/claude-haiku-4.5",
      "timeoutSeconds": 90,
      "lightContext": false,
      "deleteAfterRun": false          // useful with --at one-shots
    }
  ]
}
```

Fields map 1:1 to `openclaw cron add` flags (see
`openclaw cron add --help`). The persona system uses cron rather than
OpenClaw heartbeats because heartbeats require a delivery channel —
without one they skip every tick (`status: "skipped", reason:
"target-none"` / `"no-target"`) and never invoke the model. Cron is
the documented mechanism for "run the agent on a schedule with no
delivery target" — see
[openclaw docs/automation/cron-vs-heartbeat](https://openclaw.ai/automation/cron-vs-heartbeat).

## Current personas

| Name      | Role                                | Email                  |
| --------- | ----------------------------------- | ---------------------- |
| `lobby`   | Friendly naive lobster support bot. | `lobby@gmail.com`      |
| `mrsmith` | Adversarial agent attacking Lobby.  | `mrsmith@gmail.com`    |
| `eve`     | Generic adversarial red-team agent. | (no fixed mailbox)     |

`lobby` and `mrsmith` both run a 2-minute cron job driving the testnet
mail node ([docs/design_documents/mail-server-design.md](../../docs/design_documents/mail-server-design.md)).
`eve` ships with an empty `cron.json` (`{"jobs": []}`) — she only acts
when prompted.

## Adding a new persona

1. Create `configs/personas/<NAME>/` with the files above.
2. Required files: `IDENTITY.md`, `SOUL.md`, `AGENTS.md`.
3. Optional `cron.json` if the persona should run on a schedule.
4. Verify on a VM with
   `bash deploy/aws-deploy.sh openclaw install --persona <NAME> ...`.
