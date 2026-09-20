package cli

import (
	"os"
	"testing"
)

func TestLoadDotEnv(t *testing.T) {
	p := writeTemp(t, ".env", "# comment\nDD_API_KEY=abc123   # required\nDD_SUBDOMAIN=\"myorg\"\nexport DD_SITE='datadoghq.eu'\nAOC_TEST_PRESET=fromfile\nNOEQUALS\n")
	t.Setenv("AOC_TEST_PRESET", "fromenv")
	for _, k := range []string{"DD_API_KEY", "DD_SUBDOMAIN", "DD_SITE"} {
		os.Unsetenv(k)
		t.Cleanup(func() { os.Unsetenv(k) })
	}
	loadDotEnv(p)
	for k, want := range map[string]string{"DD_API_KEY": "abc123", "DD_SUBDOMAIN": "myorg", "DD_SITE": "datadoghq.eu", "AOC_TEST_PRESET": "fromenv"} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}
