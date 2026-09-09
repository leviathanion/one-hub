# Stream large request bodies without proxy-level retry

> ADR-0030继续采用该执行边界：Billing Owner只调用一次配置好的requester，任何返回错误都不会触发第二次Application Submission或provider failover；它不承诺缺少provider幂等契约时的provider-observable at-most-once。

Operations that do not need to inspect or transform a request body forward it as a one-shot stream instead of pre-reading a large replay copy or writing a temporary file. Once the configured requester returns an error, the relay treats provider execution as ambiguous and does not make another application-level call or start provider failover. Replayable bodies and idempotency headers do not widen that execution right; stronger external-side-effect guarantees require the provider's own idempotency contract.

Generation requests that require bounded policy or translation inspection are rejected when their body exceeds that inspection bound. Supporting those rare oversized requests would require disk spooling or bypassing an applicable policy, neither of which justifies the added complexity here.
