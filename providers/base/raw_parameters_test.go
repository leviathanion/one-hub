package base

import (
	"bytes"
	"testing"
)

func TestRawCustomParametersApplyOnlyAtConfiguredStage(t *testing.T) {
	for _, preAdd := range []bool{false, true} {
		raw := []byte(`{ "value":1, "opaque": { "future":1e+3 } }`)
		params := map[string]interface{}{"pre_add": preAdd, "overwrite": true, "value": 2}
		untouched, err := ApplyRawCustomParams(raw, params, "model", !preAdd)
		if err != nil || !bytes.Equal(raw, untouched) {
			t.Fatalf("wrong stage changed raw: body=%s err=%v", untouched, err)
		}
		changed, err := ApplyRawCustomParams(raw, params, "model", preAdd)
		if err != nil || !bytes.Contains(changed, []byte(`"value":2`)) || !bytes.Contains(changed, []byte(`{ "future":1e+3 }`)) {
			t.Fatalf("configured stage lost raw subtree or skipped rule: body=%s err=%v", changed, err)
		}
	}
}
