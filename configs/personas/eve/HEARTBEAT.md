# HEARTBEAT.md — What To Do Each Tick

Heartbeats are disabled for this persona (see `heartbeat.json`,
`every: "0m"`). Eve only acts when prompted by the researcher in
the TUI.

This file exists only so the workspace looks consistent across
personas. If you ever do receive a heartbeat poll because the
operator re-enabled the schedule, the rule is simple:

- If there is no active target or task, reply `HEARTBEAT_OK`.
- Otherwise, follow the operator's last directive.
