package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

const maximumAccountPriority = 1000000

type accountPriorities map[string]int
type accountPriorityInteger int

func (p *accountPriorities) UnmarshalJSON(raw []byte) error {
	var values map[string]accountPriorityInteger
	if err := json.Unmarshal(raw, &values); err != nil {
		return err
	}
	if values == nil {
		return fmt.Errorf("account_priority must be an object; use {} to remove rules")
	}
	out := cloneAccountPriority(*p)
	for id, value := range values {
		out[id] = int(value)
	}
	*p = out
	return nil
}

func (v *accountPriorityInteger) UnmarshalJSON(raw []byte) error {
	var value *int
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	if value == nil {
		return fmt.Errorf("account priority must be an integer")
	}
	if *value < -maximumAccountPriority || *value > maximumAccountPriority {
		return fmt.Errorf("account priority must be between %d and %d", -maximumAccountPriority, maximumAccountPriority)
	}
	*v = accountPriorityInteger(*value)
	return nil
}

func accountPriorityField() map[string]any {
	return map[string]any{"type": "integer", "minimum": -maximumAccountPriority, "maximum": maximumAccountPriority, "title": "Account priority", "description": "Higher values are used first among eligible accounts. Default 0 preserves dynamic ranking. Reserves, freshness and paid-use checks always apply; affinity and parallel work stay within the highest available tier."}
}

func validateAccountPriority(values map[string]int) error {
	if len(values) > 500 {
		return fmt.Errorf("too many account priority rules")
	}
	for id, n := range values {
		if id == "" || len(id) > 512 || strings.TrimSpace(id) != id || strings.ContainsAny(id, "\r\n\t") || n < -maximumAccountPriority || n > maximumAccountPriority {
			return fmt.Errorf("invalid account priority")
		}
	}
	return nil
}

func cloneAccountPriority(values map[string]int) map[string]int {
	out := map[string]int{}
	for id, n := range values {
		out[id] = n
	}
	return out
}

type modelProviderPriorities map[string]map[string]int

func (p *modelProviderPriorities) UnmarshalJSON(raw []byte) error {
	var values map[string]map[string]accountPriorityInteger
	if err := json.Unmarshal(raw, &values); err != nil {
		return err
	}
	if values == nil {
		return fmt.Errorf("model_provider_priority must be an object; use {} to remove rules")
	}
	out := cloneModelProviderPriority(*p)
	for model, providers := range values {
		if providers == nil {
			return fmt.Errorf("model provider priority map must be an object")
		}
		target := map[string]int{}
		for provider, value := range providers {
			target[provider] = int(value)
		}
		out[model] = target
	}
	*p = out
	return nil
}

func modelProviderPrioritySchema() map[string]any {
	return map[string]any{
		"type":          "object",
		"maxProperties": 200,
		"propertyNames": map[string]any{
			"minLength": 1,
			"maxLength": 256,
			"pattern":   `^[!-~](?:[ -~]*[!-~])?$`,
		},
		"additionalProperties": map[string]any{
			"type":          "object",
			"maxProperties": 50,
			"propertyNames": map[string]any{
				"minLength": 1,
				"maxLength": 64,
				"pattern":   `^[!-~]+$`,
			},
			"additionalProperties": accountPriorityField(),
		},
	}
}

func isValidModelPriorityKey(model string) bool {
	if model == "" || utf8.RuneCountInString(model) > 256 {
		return false
	}
	for i := 0; i < len(model); i++ {
		b := model[i]
		if b < 0x20 || b > 0x7e {
			return false
		}
	}
	return model[0] != ' ' && model[len(model)-1] != ' '
}

func isValidProviderPriorityKey(provider string) bool {
	if provider == "" || utf8.RuneCountInString(provider) > 64 {
		return false
	}
	for i := 0; i < len(provider); i++ {
		b := provider[i]
		if b < 0x21 || b > 0x7e {
			return false
		}
	}
	return true
}

func validateModelProviderPriority(values map[string]map[string]int) error {
	if len(values) > 200 {
		return fmt.Errorf("too many model provider priority rules")
	}
	for model, providers := range values {
		if !isValidModelPriorityKey(model) {
			return fmt.Errorf("invalid model provider priority model")
		}
		if providers == nil || len(providers) > 50 {
			return fmt.Errorf("invalid model provider priority rule set")
		}
		for provider, n := range providers {
			if !isValidProviderPriorityKey(provider) || n < -maximumAccountPriority || n > maximumAccountPriority {
				return fmt.Errorf("invalid model provider priority")
			}
		}
	}
	return nil
}

func cloneModelProviderPriority(values map[string]map[string]int) map[string]map[string]int {
	out := map[string]map[string]int{}
	for model, providers := range values {
		copied := map[string]int{}
		for provider, n := range providers {
			copied[provider] = n
		}
		out[model] = copied
	}
	return out
}

func effectiveCandidatePriority(p policy, model string, c candidate) int {
	if byProvider, ok := p.ModelProviderPriority[model]; ok {
		if n, exists := byProvider[c.Provider]; exists {
			return n
		}
	}
	return p.AccountPriority[c.ID]
}
