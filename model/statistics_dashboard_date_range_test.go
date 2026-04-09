package model

import (
	"testing"
	"time"
)

func TestNormalizeDashboardDateRangeValidatesShape(t *testing.T) {
	testCases := []struct {
		name          string
		dateRange     DashboardDateRange
		expected      DashboardDateRange
		expectedInErr string
	}{
		{
			name: "valid snapshot",
			dateRange: DashboardDateRange{
				Start: "2026-03-31",
				End:   "2026-04-06",
				Today: "2026-04-06",
			},
			expected: DashboardDateRange{
				Start: "2026-03-31",
				End:   "2026-04-06",
				Today: "2026-04-06",
			},
		},
		{
			name: "allows historical self-consistent range",
			dateRange: DashboardDateRange{
				Start: "2026-01-01",
				End:   "2026-04-06",
				Today: "2026-04-06",
			},
			expected: DashboardDateRange{
				Start: "2026-01-01",
				End:   "2026-04-06",
				Today: "2026-04-06",
			},
		},
		{
			name: "rejects end that drifts past today",
			dateRange: DashboardDateRange{
				Start: "2026-03-31",
				End:   "2026-04-07",
				Today: "2026-04-06",
			},
			expectedInErr: "dateRange.end must equal dateRange.today",
		},
		{
			name: "allows short self-consistent range",
			dateRange: DashboardDateRange{
				Start: "2026-04-01",
				End:   "2026-04-06",
				Today: "2026-04-06",
			},
			expected: DashboardDateRange{
				Start: "2026-04-01",
				End:   "2026-04-06",
				Today: "2026-04-06",
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			normalized, err := NormalizeDashboardDateRange(tc.dateRange)
			if tc.expectedInErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.expectedInErr)
				}
				if err.Error() != tc.expectedInErr {
					t.Fatalf("expected error %q, got %q", tc.expectedInErr, err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("expected date range to normalize, got %v", err)
			}
			if normalized != tc.expected {
				t.Fatalf("expected normalized range %+v, got %+v", tc.expected, normalized)
			}
		})
	}
}

func TestValidateDashboardDateRangeMatchesSnapshot(t *testing.T) {
	now := time.Date(2026, 4, 6, 15, 4, 5, 0, time.Local)

	if err := ValidateDashboardDateRangeMatchesSnapshot(DashboardDateRange{
		Start: "2026-03-31",
		End:   "2026-04-06",
		Today: "2026-04-06",
	}, now); err != nil {
		t.Fatalf("expected current snapshot to validate, got %v", err)
	}

	err := ValidateDashboardDateRangeMatchesSnapshot(DashboardDateRange{
		Start: "2026-01-01",
		End:   "2026-01-07",
		Today: "2026-01-07",
	}, now)
	if err == nil {
		t.Fatalf("expected historical snapshot mismatch to fail")
	}
	if !IsDashboardSnapshotMismatchError(err) {
		t.Fatalf("expected snapshot mismatch error, got %T", err)
	}
	expectedErr := "dateRange must match the current dashboard snapshot (expected start=2026-03-31 end=2026-04-06 today=2026-04-06, got start=2026-01-01 end=2026-01-07 today=2026-01-07)"
	if err.Error() != expectedErr {
		t.Fatalf("expected error %q, got %q", expectedErr, err.Error())
	}
}
