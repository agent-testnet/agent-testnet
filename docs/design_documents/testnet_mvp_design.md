# Agent Testnet — MVP Design

## Goal

Provide a sandboxed internet-like environment where:
- AI agents can interact safely
- Independent testnet nodes expose non-production services
- Nodes share a common DNS + trust layer
- Agent traffic is fully controlled and isolated
- Node-to-node traffic is NOT enforced through a central fabric

---

## Core Design Decision

Only agent traffic is strictly controlled.
Testnet nodes are federated participants and may communicate directly.

---

## Architecture Overview

Control Plane (registry, DNS, CA)
        ↓
Testnet DNS
        ↓
Agent Routing Gateway (only for agent traffic)
        ↓
Testnet Nodes (Google, Example, etc.)
        ↑
Agent Sandbox (MicroVM)

---

## Components

### Control Plane
- Node registration
- Domain assignment
- DNS generation
- Certificate issuance

### Testnet DNS
- Resolves all domains to testnet endpoints
- Shared across agents and nodes

### Agent Gateway
- Intercepts all agent traffic
- Blocks real internet
- Routes requests to nodes
- Logs traffic

### Testnet Nodes
- External or internal services
- Register domains
- Participate in DNS
- Can communicate directly with other nodes

### Agent Sandbox
- Runs agent in isolated environment
- Forces traffic through gateway

---

## Network Model

Agent → Gateway → Node (controlled)

Node → Node (direct, not controlled)

Node → Internet (allowed in MVP, not controlled)

---

## Trust Model

Guaranteed:
- Agent isolation
- Controlled DNS
- Domain mapping

Not guaranteed:
- Node isolation
- No side channels
- No external node communication

---

## TLS

- Testnet root CA
- Certificates issued per domain
- Installed in agent and optionally nodes

---

## Example Flow

Agent → google.com → Gateway → Google node → response

Google node → example.com → direct → Example node → response

---

## MVP Scope

Included:
- Control plane API
- DNS server
- Agent gateway
- MicroVM sandbox
- Basic nodes

Excluded:
- Node traffic enforcement
- Full observability
- Complex routing

---

## Limitations

- Nodes may access real internet
- Limited observability (agent-only)
- Semi-trusted nodes

---

## Summary

A sandboxed agent environment connected to a federated testnet where nodes share DNS and domains but are not required to route through a central fabric.
