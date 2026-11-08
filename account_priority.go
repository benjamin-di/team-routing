package main

import (
	"fmt"
	"strings"
)

const maximumAccountPriority = 1000000

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
