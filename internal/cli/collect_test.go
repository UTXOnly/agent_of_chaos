package cli

import (
	"testing"
)

func TestSummarizeAgentLog(t *testing.T) {
	log := `2026-09-20 17:23:14 UTC | CORE | INFO | (pkg/logs/launchers/file/launcher.go:120 in Start) | starting
2026-09-20 17:23:14 UTC | CORE | WARN | (pkg/api/security/cert/cert_getter.go:37 in getCertFilepath) | IPC cert/key created or retrieved next to auth_token_file_path location: /etc/datadog-agent/ipc_cert.pem
2026-09-20 17:23:15 UTC | CORE | ERROR | (comp/logs/agent/sender.go:88 in send) | Post "http://localhost:8282/api/v2/logs": connection refused (attempt 3)
2026-09-20 17:23:16 UTC | CORE | ERROR | (comp/logs/agent/sender.go:88 in send) | Post "http://localhost:8282/api/v2/logs": connection refused (attempt 4)
2026-09-20 17:23:16 UTC | TRACE | WARN | (pkg/trace/api/api.go:10 in x) | slow 1234ms
not a structured line
`
	s := summarizeAgentLog([]byte(log), 20000)
	if s.Lines != 6 || s.Errors != 2 || s.Warnings != 2 || s.Truncated {
		t.Fatalf("%+v", s)
	}
	if len(s.Top) != 3 || s.Top[0].Count != 2 || s.Top[0].Name != `ERROR | CORE | Post "http://localhost:#/api/v2/logs": connection refused (attempt #)` {
		t.Errorf("top: %+v", s.Top)
	}
	if s2 := summarizeAgentLog([]byte(log), 6); !s2.Truncated {
		t.Error("hitting the tail limit should flag truncation")
	}
}
