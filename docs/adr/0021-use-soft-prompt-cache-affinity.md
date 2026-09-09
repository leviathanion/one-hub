# Use soft Prompt Cache Affinity

Chat and Responses share the existing affinity mechanism and prefer the prior channel for the same group, canonical model, and `prompt_cache_key`, with the existing one-hour default lifetime. The preference may fall back to ordinary scheduling when its channel is unavailable and records a miss, preserving availability because prompt caching is a cost and latency optimization rather than resource ownership; existing administrator-defined affinity rules remain available.
