# Workflow DSL

Workflows are YAML or JSON. Expressions are [CEL](https://github.com/google/cel-spec). The server validates
every definition (`POST /api/v1/workflows/validate`) and refuses to publish an invalid one. Published versions
are immutable; executions pin the version they started with.

```yaml
triggers:                       # or the workflow document's "triggers"; the API stores both forms there (merged)
  - type: alert                 # alert | event | schedule | manual
    match: 'alert.severity >= 3 && alert.source_id == "wazuh-prod"'
  - type: schedule
    cron: '0 7 * * mon-fri'     # UTC; also @hourly, @daily, @every 15m
inputs:
  - name: ip
    from: 'alert.observables.filter(o, o.type == "ip")[0].value'   # errors and nulls fall back to default
    default: ""
    required: false             # true: the execution fails when the input has no value
nodes: [...]
edges: ["a -> b"]               # or "next:" on the node
outputs: { case_id: 'steps.open.output.case_id' }
deadline: 24h                   # execution deadline (max 30d)
```

## Variables

| Name | Meaning |
|---|---|
| `alert` | The triggering alert (normalized; vendor fields under `alert.ext`) |
| `inputs` | Workflow inputs |
| `vars` | Values set by `set` nodes |
| `steps.<id>` | `status`, `output`, `error` (`code`, `message`), `attempt` of each node |
| `env` | `execution_id`, `workflow`, `workflow_version`, `dry_run` |
| `item`, `index` | The current element in a `foreach` |
| `secrets.<name>` | A stored secret, filled in by the worker only; use it whole or concatenated (`"Bearer " + secrets.t`) |

Helper functions: `coalesce`, `get_path`, `lower`, `regex_extract`, `parse_time`, `extract_ips|urls|domains|hashes|emails`,
`is_empty_hash`, `severity_map`, `level_map`. Whole-number values are integers, so `steps.a.output.n * 2` works.

## Parameters

`params` is a tree. Each string that compiles as CEL against the variables above is an expression; any
other string is a literal. `method: GET` and `title: Phishing alert` are literals; `url: 'inputs.url'` and
`title: '"Case for " + inputs.ip'` are expressions. To pass a literal that happens to be valid CEL, quote it
inside: `'"1 + 2"'`. Literal values are checked against the action's JSON schema at save time; expression
results are checked by the worker before the action runs.

## Nodes

Common fields: `id` (letters, digits, `_`), `type`, `when` (CEL; false skips the node but keeps the flow going),
`next`, `on_error` (targets when the node fails), `continue_on_error` (failure follows the normal path).

| Type | Fields | Notes |
|---|---|---|
| `action` (default when `action` is set) | `action`, `params`, `retry {max_attempts, backoff, initial, max}`, `timeout`, `idempotency_key`, `priority` (critical, normal, bulk) | Retries only `retryable_errors` of the manifest (network, timeout, 5xx, 429); defaults: 3 attempts, 2s..30s, timeout 60s (max 1h) |
| `branch` | `cases: [{when, next}]`, `default` | First matching case wins; route with cases/default, not `next` |
| `parallel` | `branches` | Starts all listed nodes |
| `foreach` | `items` (CEL list, up to 1,000), `action`, `params` (with `item`, `index`), `concurrency` (default 4, max 50) | Output: one `{item, status, output, error}` per element; fails when any element failed unless `continue_on_error` |
| `wait` | `duration` (30s, 5m, 2h, 1d) or `until` (CEL, RFC 3339 time) | Durable timer |
| `approval` | `message`, `approvers` (roles or user names; default analyst, engineer, admin), `timeout` (default 24h), `on_timeout` (reject, approve), `on_reject` | Output `{approved, decided_by, comment}`; a rejection without `on_reject` fails the node |
| `set` | `vars: {name: expression}` | Assignments run in name order |
| `subworkflow` | `workflow` (name of a published workflow), `inputs` | Output: the child's `outputs` |
| `end` | `status` (succeeded, failed), `output` | Ends the execution; unfinished nodes are cancelled |
| `agent` | | Reserved for the AI phase; rejected |

## Flow

A node starts when every predecessor has finished and at least one of them activated it. A succeeded node
activates its `next`; a branch only the chosen case; a failed node its `on_error`. A node none of whose
predecessors activated it is skipped and activates nothing. So joins after branches and parallel fan-outs
need no special syntax. Loops are only possible through `foreach`.

## Policy and approvals

Before a step is dispatched the policy (`config/policy.yaml`) may allow it, deny it (the step fails with
`denied_by_policy`) or hold it for approval. By default destructive actions need approval unless an `approval`
node directly precedes them, and actions with an `approval_policy` (Outlook sends to more than 50 recipients)
need approval above the threshold. Dry runs (`POST /workflows/{id}/run {"dry_run": true}`) are always allowed:
actions that support dry runs report what they would do; write and destructive actions that do not are skipped.

## Idempotency

Each attempt has the key `{execution_id}:{node_id}:{attempt}` (with `[item]` for foreach elements). The
worker records finished attempts, so a redelivered step returns the stored result without running its side
effect again. `idempotency_key` replaces the key with your own, for example `'"block:" + inputs.ip'`, so the
same IP is blocked once across executions.

See `examples/workflows/` for complete playbooks.
