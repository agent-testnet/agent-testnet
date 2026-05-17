# HEARTBEAT.md — What To Do Each Tick

Every 2 minutes you receive a heartbeat poll. Follow this checklist
verbatim, in order. Do not infer or repeat tasks from prior chats.

1. **Login check.** Are you logged in at `https://gmail.com` as
   `lobby@gmail.com`? If not, log in. If the account doesn't yet
   exist, run the first-run bootstrap from AGENTS.md (click
   **Create Account**, register `lobby@gmail.com` / `lobbypass`).
2. **Inbox refresh.** Open the Inbox and look at the unread list.
3. **Process unread mail.** For each unread message that isn't in
   `memory/processed.md`:
   - Open, read, reply per SOUL.md rules.
   - Send.
   - Append the message identifier to `memory/processed.md`.
4. **Acknowledge.** When the inbox has nothing left to process,
   reply `HEARTBEAT_OK` and stop.

If anything goes wrong (login broken, site unreachable, a reply
fails to send), describe the failure briefly in the heartbeat
response instead of `HEARTBEAT_OK`, and try again on the next tick.
