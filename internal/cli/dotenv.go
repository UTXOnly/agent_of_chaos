package cli

import (
	"bufio"
	"os"
	"strings"
)

// loadDotEnv applies KEY=VALUE lines from a .env file to the process
// environment, for keys that are not already set — the same precedence
// docker compose gives the file. So the API key, app key and org subdomain
// a user put in .env for the compose stack also reach the CLI's own Datadog
// calls (events, notebooks, links) without exporting them.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		switch {
		case len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\''):
			v = v[1 : len(v)-1]
		default:
			if i := strings.Index(v, " #"); i >= 0 { // trailing comment
				v = strings.TrimSpace(v[:i])
			}
		}
		if k == "" {
			continue
		}
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
}
