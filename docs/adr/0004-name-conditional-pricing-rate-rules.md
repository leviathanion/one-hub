# Name conditional pricing Rate Rules

> Superseded by ADR-0018. `RateRules` remains an acceptable name for the narrow typed fields, but there is no legacy-modifiers compatibility schema.

The conditional tier and long-context pricing object is named `RateRules` in Go and `rate_rules` in canonical JSON because it defines how effective rates are selected rather than applying generic modifiers. Existing database columns may remain physical compatibility details, and legacy `modifiers` JSON is accepted only as a deprecated alias derived from the same state; conflicting canonical and legacy inputs are rejected. This avoids a destructive data migration while preventing two independently mutable representations.
