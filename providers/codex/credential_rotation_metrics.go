package codex

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	credentialRotationClaims = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "codex_credential_rotation_claim_total",
		Help: "Durable Codex credential rotation claims grouped by outcome.",
	}, []string{"outcome"})
	credentialRotations = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "codex_credential_rotation_total",
		Help: "Codex credential rotation attempts grouped by terminal outcome and reason.",
	}, []string{"outcome", "reason"})
	credentialRotationCommitRetries = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "codex_credential_rotation_commit_retry_total",
		Help: "Bounded credential commit retries grouped by outcome.",
	}, []string{"outcome"})
	credentialRotationUnresolved = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "codex_credential_rotation_unresolved_total",
		Help: "Credential rotations left durably fenced grouped by reason.",
	}, []string{"reason"})
)
