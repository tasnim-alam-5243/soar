// Package audit records every mutation and secret read (spec section 13).
//
// Two kinds of actor write audit records:
//
//   - users and API keys: the api middleware records every successful mutation
//     (actor = principal name, actor_type user or system);
//   - workflows: the worker records each secret it decrypts for a step and each
//     completed write or destructive action (actor = "workflow:<name>", actor_type
//     "workflow", request_id = the step's idempotency key {execution_id}:{node_id}:{attempt}).
//
// Secret values are never part of a record, only secret and connection names.
package audit
