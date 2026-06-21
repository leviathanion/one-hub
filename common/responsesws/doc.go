// Package responsesws contains Responses WebSocket provider-side transport
// helpers and wire utilities.
//
// The native helper is intentionally provider-internal: provider openers still
// own URL/header construction, channel policy, proxy/private-IP validation and
// dialing, then pass an established wsconn.ManagedConn plus a provider adapter
// into this package. Relay-facing accounting, quota, RPM, affinity and terminal
// side effects remain owned by the ResponsesWS actor.
package responsesws
