# One Hub Refactor Blueprint

## Background

Current `one-hub` is functionally rich, but several core paths are still built around:

- global mutable state
- package-level initialization
- relay flow coupled to Gin context and provider side effects
- runtime selection/retry/cooldown logic embedded in request handlers
- scattered request/response adaptation logic

Compared with `CLIProxyAPI`, the main gap is not features. The gap is that `CLIProxyAPI` has already turned its proxy core into a reusable runtime with clear boundaries for config, lifecycle, translation, selection, and hot reload.

This blueprint focuses on borrowing those stronger designs without forcing a big-bang rewrite.

## Refactor Goals

1. Reduce global state and hidden initialization order dependencies.
2. Extract the relay core into testable services and policies.
3. Separate protocol translation from request dispatch.
4. Introduce a runtime model/channel registry as the source of truth for routing.
5. Make configuration strongly typed and reloadable.
6. Increase test coverage around runtime behavior instead of only provider handlers.

## Non-Goals

- No immediate rewrite of all providers.
- No frontend redesign in this phase.
- No database schema rewrite unless a phase explicitly requires it.
- No breaking API behavior unless covered by compatibility gates and tests.

## Target Architecture

### 1. App Lifecycle Layer

Create an `internal/app` or `app` package that owns:

- config loading
- logger setup
- database and redis lifecycle
- notifier, searcher, storage, telegram, task scheduler startup
- HTTP server construction
- shutdown hooks

Target shape:

```go
type App struct {
    Config   *config.Config
    Server   *http.Server
    DB       *gorm.DB
    Redis    redis.UniversalClient
    Runtime  *runtime.Manager
}

func New(cfg *config.Config, opts ...Option) (*App, error)
func (a *App) Start(ctx context.Context) error
func (a *App) Shutdown(ctx context.Context) error
```

This replaces the current startup pattern in `main.go`, where initialization order is implicit and spread across packages.

### 2. Strongly Typed Config Layer

Replace the current `viper`-driven package globals with:

- a central `Config` struct
- `Load(path string) (*Config, error)`
- `Apply(*Config)` only at process boundaries
- `Validate() error`
- optional config diff and redacted change log

Rules:

- runtime logic should depend on `*config.Config`, not package globals
- package globals remain only as temporary compatibility shims
- new config fields must be typed, documented, and testable

### 3. Runtime Manager Layer

Introduce a runtime manager that owns hot path state:

- channel registry
- model registry
- selector
- retry policy
- cooldown policy
- optional runtime health state

Suggested packages:

- `internal/runtime`
- `internal/runtime/selector`
- `internal/runtime/policy`
- `internal/runtime/registry`

The current `model.ChannelGroup` can become an implementation detail under this runtime layer instead of being directly touched by request handlers.

### 4. Execution Pipeline Layer

Split the current relay flow into explicit stages:

1. parse inbound request
2. normalize request metadata
3. resolve route target
4. translate request if needed
5. execute upstream call
6. translate response if needed
7. record usage and metrics
8. apply retry/cooldown policy if needed

Suggested abstractions:

```go
type ExecutionContext struct {
    RequestID string
    UserID    int
    TokenID   int
    Model     string
    Stream    bool
}

type Executor interface {
    Execute(ctx context.Context, req Request) (Response, error)
    ExecuteStream(ctx context.Context, req Request) (StreamResult, error)
}

type RetryPolicy interface {
    ShouldRetry(result Result) bool
    NextDelay(result Result, attempt int) time.Duration
}
```

This removes policy from `relay/main.go` and makes retry behavior independently testable.

### 5. Translation Pipeline Layer

Move request/response adaptation out of relay handlers into a translation system:

- request translator registry
- response translator registry
- middleware-style translation pipeline
- per-provider or per-protocol translation modules

This is the right home for:

- pre-mapping
- request body rewriting
- provider custom parameters that mutate request payload
- compatible response normalization
- OpenAI/Gemini/Claude/Codex format bridging

This will prevent `relay` from continuing to accumulate provider-specific branches.

### 6. Runtime Registry Layer

Separate two concepts that are currently mixed:

- management metadata in DB
- actual runtime availability for routing

Add a runtime registry that tracks:

- which channels support which models
- effective status after cooldown or failures
- dynamic capabilities such as streaming support
- cached available models per group/user scope

The DB remains the persistence layer. The runtime registry becomes the routing truth.

### 7. Router and Controller Boundary Cleanup

Current API routing is very large and tightly coupled to concrete controller functions.

Refactor toward:

- smaller route modules by domain
- service layer between controller and model
- request DTO and response DTO structs
- controller methods that orchestrate, not implement business rules

Suggested domains:

- auth
- user
- token
- channel
- pricing
- analytics
- payment
- integration

### 8. Test Strategy Upgrade

Shift tests toward runtime behaviors:

- config load and validation
- selector behavior
- retry and cooldown decisions
- translation correctness
- hot reload diff application
- model registry updates
- relay compatibility snapshots

The core rule is:

> every new abstraction must earn its keep with focused tests.

## Suggested Delivery Phases

### Phase 0: Stabilize and Measure

Deliverables:

- freeze new refactors outside the selected scope
- add baseline relay tests around existing behavior
- add startup smoke tests
- document current initialization order and coupling points

Exit criteria:

- core relay flows have regression coverage
- startup path has at least one smoke test

### Phase 1: App Shell and Typed Config

Deliverables:

- introduce `Config` struct and loader
- build `App` object to own startup and shutdown
- move `main.go` to thin bootstrap
- keep compatibility shims for existing packages

Exit criteria:

- `main.go` mostly calls `config.Load`, `app.New`, `app.Start`
- most startup dependencies no longer initialize themselves implicitly

### Phase 2: Selector and Policy Extraction

Deliverables:

- extract channel selection from relay handlers
- extract retry logic into `RetryPolicy`
- extract cooldown logic into `CooldownPolicy`
- move skip lists and transient routing state out of Gin keys where possible

Exit criteria:

- `relay/main.go` becomes orchestration only
- selector and retry behavior are unit-tested without Gin

### Phase 3: Translation Pipeline

Deliverables:

- add translator registry and pipeline
- migrate pre-mapping logic
- migrate compatible response shaping
- define provider-level translation hooks

Exit criteria:

- relay no longer rewrites bodies directly
- protocol adaptation is testable in isolation

### Phase 4: Runtime Registry and Hot Reload

Deliverables:

- add runtime model/channel registry
- support config diff and redacted reload logs
- update runtime state incrementally instead of broad reloads

Exit criteria:

- available-model computation reads from runtime registry
- hot reload no longer depends on restarting broad subsystems

### Phase 5: Controller and Route Cleanup

Deliverables:

- split large route files
- move business logic from controllers into services
- define DTOs and validation boundaries

Exit criteria:

- controllers are thinner
- route registration is domain-oriented

### Phase 6: Hardening and Cleanup

Deliverables:

- remove deprecated config globals
- remove legacy relay branches replaced by pipeline components
- add benchmark and regression suites for selector and relay paths

Exit criteria:

- runtime path uses new abstractions end to end
- compatibility shims are reduced to near zero

## Migration Rules

- prefer strangler migration over rewrite
- keep old behavior behind compatibility adapters during each phase
- each phase must end in a releasable state
- avoid cross-cutting edits without adding tests first
- if a package still depends on package globals, treat it as technical debt and isolate it behind adapter functions

## Immediate Refactor Entry Points

The best first cuts are:

1. `main.go` startup extraction
2. `common/config` typed config introduction
3. `relay/main.go` selector and retry extraction
4. `model.ChannelGroup` runtime wrapper
5. available model calculation moved behind a registry interface

## Risks

- hidden package init behavior may break startup order
- some controllers may rely on side effects from model or common packages
- request body reuse logic may have compatibility edge cases in streaming paths
- provider-specific quirks may be embedded in relay code rather than provider code

## Risk Controls

- add characterization tests before moving logic
- change one axis at a time: startup, then policy, then translation
- keep adapter layers for one release cycle where practical
- benchmark relay path before and after selector extraction

## Definition of Success

This refactor succeeds when:

- startup is owned by one app/service object
- runtime selection and retry policies are testable without Gin
- translation logic lives outside relay handlers
- available models come from runtime registry, not scattered queries
- config is typed, validated, and ready for safe reload
- new features can be added by extending modules, not editing core relay flow
