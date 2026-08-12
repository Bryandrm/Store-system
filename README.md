# store-system

Sistema de ventas para una tienda chica: registro rápido de ventas, inventario,
reabastecimiento, fiado y saldo a favor. PWA instalable que **funciona sin
señal** y sincroniza entre dispositivos.

Los productos son semillas, maní, maní dulce, maní japonés, marañón, semillas de
pepitoria, chocolates y churros. El punto de venta suele estar sin cobertura, así
que trabajar offline no es una comodidad: es el requisito principal.

## La idea en una frase

> **Todo dato que toca plata o stock es una fila inmutable. Todo número que la UI
> muestra es un `SUM()`. Los dispositivos offline solo hacen `INSERT`.**

De ahí sale la propiedad central: **ningún conflicto de sincronización puede
existir por construcción**. Y no queda librado a la disciplina del código — el
rol con el que corre la API **no tiene permiso de `UPDATE` ni `DELETE`** sobre
ninguna tabla transaccional.

## Stack

| Capa | Elección |
|---|---|
| Backend | Go 1.26 · `net/http` de stdlib · `pgx` · `sqlc` · `goose` |
| Base | PostgreSQL 18 con `data-checksums` |
| Frontend | React 19 · Vite · TypeScript estricto · Tailwind v4 · Zustand |
| Offline | IndexedDB · service worker propio · cola de operaciones |
| Infra | Cloudflare Pages (PWA) + GCP e2-micro (API) + Caddy |

Sin ORM, sin framework HTTP, sin librería de componentes.

## Arrancar

```bash
# 1. la base de desarrollo (puerto 5433, para no chocar con otro Postgres)
docker compose -f compose.dev.yml up -d

# 2. los tests, que crean y borran sus propias bases efímeras
go test ./... -race
```

Requisitos: Go 1.26+, Docker o Colima, Node 22+ (cuando exista el frontend).

## Documentación

| Archivo | Qué contiene |
|---|---|
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | Los cuatro diagramas y de dónde viene cada decisión |
| `docs/DECISIONS.md` | ADRs *(pendiente)* |
| `docs/SYNC.md` | El protocolo y su argumento de correctitud *(pendiente)* |
| `docs/DEPLOY.md` | Despliegue y runbook de restauración *(pendiente)* |
| `docs/GOTCHAS.md` | Bitácora numerada de trampas ya pagadas *(pendiente)* |

## Estado

En desarrollo, milestone 1 (**"Vender y ver el día"**).

| Componente | Estado |
|---|---|
| Esquema, ledgers y vistas derivadas | ✅ migrado y probado |
| Blindaje append-only por permisos | ✅ 12 tablas verificadas en CI |
| Aritmética de dinero + fixture compartido | ✅ |
| Harness de base efímera | ✅ |
| Contrato HTTP | ✅ |
| Autenticación | ⬜ |
| Ventas y sincronización | ⬜ |
| PWA | ⬜ |

## Convenciones

Commits convencionales con el módulo como *scope*, para que el changelog se
genere agrupado por módulo:

```
feat(ventas):     registro rápido con teclado numérico
fix(sync):        la cola no reintentaba tras un 401
feat(inventario): alerta de stock bajo
```

Scopes: `ventas` · `inventario` · `fiado` · `reabastecimiento` · `perfiles` ·
`sync` · `api` · `infra` · `docs`.

Identificadores en inglés, prosa y textos de usuario en español.
