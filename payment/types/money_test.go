package types

import "testing"

func TestStrictMoneyBoundary(t *testing.T) {
	for _, test := range []struct {
		raw   string
		minor int64
	}{{"0.29", 29}, {"618.98", 61898}, {"1", 100}, {"1.2", 120}} {
		m, err := ParseMoney(test.raw, "CNY")
		if err != nil || m.Minor != test.minor {
			t.Fatalf("%s => %+v %v", test.raw, m, err)
		}
	}
	for _, raw := range []string{"", "0.290", "NaN", "1e2", "-1", " 1", "1.", ".2"} {
		if _, err := ParseMoney(raw, "CNY"); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	if _, err := ParseMoney("1", "USDT"); err == nil {
		t.Fatal("unsupported currency")
	}
	zero, err := ParseMoney("0", "CNY")
	if err != nil || zero.Validate() == nil {
		t.Fatal("zero must parse separately but cannot fund positive order")
	}
}
