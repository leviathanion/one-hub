package safty

import (
	"strconv"
	"testing"

	"one-api/common/config"
	"one-api/safty/types"
)

type fixedChecker struct {
	name string
	safe bool
}

func (c *fixedChecker) Name() string { return c.name }
func (c *fixedChecker) Init() error  { return nil }
func (c *fixedChecker) Check(string) (types.CheckResult, error) {
	return types.CheckResult{IsSafe: c.safe}, nil
}

func TestCheckContentReadsCurrentSafetyConfiguration(t *testing.T) {
	originalManager := config.GlobalOption
	originalTools := Tools
	t.Cleanup(func() {
		config.GlobalOption = originalManager
		Tools = originalTools
	})

	manager := config.NewOptionManager()
	enabled := false
	toolName := "Reject"
	manager.RegisterBool("EnableSafe", &enabled)
	manager.RegisterString("SafeToolName", &toolName)
	config.GlobalOption = manager
	Tools = map[string]SaftyTool{
		"Reject": &fixedChecker{name: "Reject", safe: false},
		"Allow":  &fixedChecker{name: "Allow", safe: true},
	}

	publishSafetyOptions(t, manager, 1, true, "Reject")
	result, err := CheckContent("content")
	if err != nil {
		t.Fatalf("CheckContent returned error: %v", err)
	}
	if result.IsSafe {
		t.Fatal("CheckContent should use the currently selected rejecting tool")
	}

	publishSafetyOptions(t, manager, 2, true, "Allow")
	result, err = CheckContent("content")
	if err != nil {
		t.Fatalf("CheckContent returned error after tool update: %v", err)
	}
	if !result.IsSafe {
		t.Fatal("CheckContent should observe the newly selected tool")
	}

	publishSafetyOptions(t, manager, 3, false, "Reject")
	result, err = CheckContent("content")
	if err != nil {
		t.Fatalf("CheckContent returned error after disabling safety: %v", err)
	}
	if !result.IsSafe {
		t.Fatal("disabled safety checks should return safe")
	}

	result, err = CheckContentByToolName("Reject", "content")
	if err != nil {
		t.Fatalf("CheckContentByToolName returned error while disabled: %v", err)
	}
	if !result.IsSafe {
		t.Fatal("CheckContentByToolName should observe the current disabled state")
	}
}

func publishSafetyOptions(t *testing.T, manager *config.OptionManager, version int64, enabled bool, toolName string) {
	t.Helper()
	published, err := manager.PublishRuntimeOverrides(version, map[string]string{
		"EnableSafe":   strconv.FormatBool(enabled),
		"SafeToolName": toolName,
	})
	if err != nil {
		t.Fatalf("publish safety options: %v", err)
	}
	if !published {
		t.Fatalf("safety options version %d was not published", version)
	}
}
