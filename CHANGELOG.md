# Changelog

Generated from conventional commits with [git-cliff](https://git-cliff.org).
Grouped by module: the scope of each commit is the module it touched.

## Unreleased



### Chores

- **infra**: Scaffold the Go project and the development database

### Documentation

- Architecture diagrams and implementation status

### Features

- **api**: Initial schema with append-only ledgers enforced by grants
- **api**: Money arithmetic with a fixture shared with TypeScript
- **api**: Connection pool, transaction helper and throwaway test database
- **api**: Single HTTP response contract
- **profiles**: Opaque bearer tokens with argon2id and login rate limiting
- **sales**: Record a sale as one atomic transaction
- **sync**: Transaction-id watermark feed and idempotent apply
- **api**: Runnable server with bootstrap and sync endpoints
- **sales**: Client-side money arithmetic mirroring the server
- **sync**: Local replica, single write path and operation outbox
- **sync**: Synchronization engine with failure classification
- **sales**: Optimistic local sale recording with UUIDv7 ordering
- **sales**: Sell screen, day view and installable PWA shell

### Fixes

- **infra**: Add @types/node so the typecheck gate passes
