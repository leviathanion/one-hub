package config

import "encoding/json"

type GeminiSettings struct {
	OpenThink map[string]bool
}

var GeminiSettingsInstance = GeminiSettings{
	OpenThink: map[string]bool{},
}

func init() {
	GlobalOption.RegisterCustomOptionWithValidator("GeminiOpenThink", func() string {
		return GeminiSettingsInstance.GetOpenThinkJSONString()
	}, func(value string) error {
		return GeminiSettingsInstance.SetOpenThink(value)
	}, func(value string) error {
		return ValidateGeminiOpenThink(value)
	}, OptionMetadata{
		Visibility: OptionVisibilityPublic,
	}, "")
}

func ValidateGeminiOpenThink(data string) error {
	if data == "" {
		return nil
	}

	var openThink map[string]bool
	return json.Unmarshal([]byte(data), &openThink)
}

func (c *GeminiSettings) SetOpenThink(data string) error {
	if data == "" {
		c.OpenThink = map[string]bool{}
		return nil
	}

	var openThink map[string]bool
	if err := json.Unmarshal([]byte(data), &openThink); err != nil {
		return err
	}
	c.OpenThink = openThink
	return nil
}

func (c *GeminiSettings) GetOpenThink(model string) bool {
	if openThink, ok := c.OpenThink[model]; ok {
		return openThink
	}
	return false
}

func (c *GeminiSettings) GetOpenThinkJSONString() string {
	str, err := json.Marshal(c.OpenThink)
	if err != nil {
		return ""
	}
	return string(str)
}
