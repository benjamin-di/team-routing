package main

import "strings"

func normalizedQuotaWindow(provider, window, label string) string {
	if provider != "antigravity" {
		return window
	}
	check := strings.ToLower(strings.TrimSpace(window + " " + label))
	compact := strings.NewReplacer(" ", "", "-", "", "_", "").Replace(check)
	if strings.Contains(check, "week") || compact == "7d" {
		return "weekly"
	}
	for _, value := range []string{window, label} {
		value = strings.NewReplacer(" ", "", "-", "", "_", "").Replace(strings.ToLower(strings.TrimSpace(value)))
		switch value {
		case "5h", "5hr", "5hrs", "5hour", "5hours", "fivehour", "fivehours":
			return "five_hour"
		}
	}
	if window == "" {
		return label
	}
	return window
}

// Normalize cached snapshots too, so an update does not leave legacy 5h data
// misranked until the next provider poll. Clone shared cache slices/maps first.
func normalizeQuotaSnapshot(provider string, q snapshot) snapshot {
	if provider != "antigravity" {
		return q
	}
	groups := make([]group, len(q.Groups))
	rates := map[string]float64{}
	for key, value := range q.Rates {
		rates[key] = value
	}
	costs := map[string]float64{}
	for key, value := range q.RequestCosts {
		costs[key] = value
	}
	for i, g := range q.Groups {
		groups[i] = g
		groups[i].Buckets = append([]bucket(nil), g.Buckets...)
		for j, b := range g.Buckets {
			window := normalizedQuotaWindow(provider, b.Window, "")
			groups[i].Buckets[j].Window = window
			if window != b.Window {
				old, next := g.Name+"|"+b.Window, g.Name+"|"+window
				if rate, ok := rates[old]; ok {
					rates[next] = max(rates[next], rate)
					delete(rates, old)
				}
				if cost, ok := costs[old]; ok {
					costs[next] = max(costs[next], cost)
					delete(costs, old)
				}
			}
		}
	}
	q.Groups, q.Rates = groups, rates
	if len(costs) > 0 {
		q.RequestCosts = costs
	}
	return q
}
