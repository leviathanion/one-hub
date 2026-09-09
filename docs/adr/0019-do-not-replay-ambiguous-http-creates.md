# Do not replay ambiguous HTTP creates

> ADR-0030保留“不重放”边界，但取代本文的“歧义时不退款”计费规则：存在Authoritative Provider Usage才Confirm，否则Cancel并返回，不换渠道或重放同一Work Action。

An HTTP create whose transport outcome is ambiguous is surfaced without retry: the provider may already have generated output, invoked tools, charged the shared account, or created a stored resource. Billing Owners follow ADR-0030 and never switch channel after submission claim; Authoritative Provider Usage confirms the customer charge, while its absence cancels the reservation even though one-hub might still bear provider cost. Body replayability and an uncommitted downstream response do not prove replay safe.
