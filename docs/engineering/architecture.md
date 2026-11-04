# Architecture: ninzo-backend

Version: 0.0.1 · Updated: 2026-11-04

## 1. System overview
What the system is, its boundaries, who/what it talks to. One context diagram.

## 2. Component architecture

### 2.1 Components
Deployable units and what each one does.

### 2.2 Modules / features
Each module: purpose, owned data, public surface, dependencies. Rule: modules talk only through their public surface.

### 2.3 Models
Core entities, their fields, relationships and state machines.

### 2.4 API layer
Endpoints or events exposed and consumed. Link to the contract file. Conventions: versioning, errors, idempotency, auth.

### 2.5 Libraries / modules (shared code)
Shared packages, what they may and may not contain.

## 3. Changes
Every architecture change is recorded as a decision in `docs/decisions/` and as an entry in `CHANGELOG.md`.
This section lists the current structure only; history goes to section 4.

## 4. Overview and project history
Dated list of significant changes, each linking to its decision and changelog entry.

| Date | Change | Decision | Changelog |
|---|---|---|---|
