package main

import "testing"

func TestPluginConfigFormats(t *testing.T) {
	t.Cleanup(func() { applyConfig(nil) })
	for _, raw := range []string{
		"state_dir: /var/lib/plugin-state\nmanagement_url: https://localhost:8317/\n",
		`{"enabled":true,"state_dir":"/var/lib/plugin-state","management_url":"https://localhost:8317/"}`,
		`{enabled: true, state_dir: /var/lib/plugin-state, management_url: 'https://localhost:8317/'}`,
	} {
		t.Run(raw, func(t *testing.T) {
			if err := applyConfig([]byte(raw)); err != nil {
				t.Fatal(err)
			}
			if got := statePath("team-routing.json"); got != "/var/lib/plugin-state/team-routing.json" {
				t.Fatalf("state path = %q", got)
			}
			if got := callerFromRequest(nil).BaseURL; got != "https://localhost:8317" {
				t.Fatalf("management URL = %q", got)
			}
		})
	}
}

func TestInvalidPluginConfigKeepsSettings(t *testing.T) {
	t.Cleanup(func() { applyConfig(nil) })
	if err := applyConfig([]byte("state_dir: /var/lib/plugin-state")); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"state_dir: [", "state_dir: {}"} {
		if err := applyConfig([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid config %q", raw)
		}
		if got := statePath("team-routing.json"); got != "/var/lib/plugin-state/team-routing.json" {
			t.Fatalf("changed settings on error: %q", got)
		}
	}
	if err := applyConfig(nil); err != nil {
		t.Fatal(err)
	}
	if got := statePath("team-routing.json"); got != "team-routing.json" {
		t.Fatalf("empty config did not reset: %q", got)
	}
}
