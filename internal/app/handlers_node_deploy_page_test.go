package app

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestDerivePanelMTLSURL(t *testing.T) {
	tests := []struct {
		name          string
		panelPublic   string
		mtlsPublicURL string
		mtlsAddr      string
		want          string
		wantOK        bool
	}{
		{
			name:          "prefer explicit mtls url",
			panelPublic:   "https://proxy.example.com",
			mtlsPublicURL: "https://mtls.example.com:8443",
			mtlsAddr:      ":8443",
			want:          "https://mtls.example.com:8443",
			wantOK:        true,
		},
		{
			name:          "accept mtls url without scheme",
			panelPublic:   "https://proxy.example.com",
			mtlsPublicURL: "mtls.example.com:9443",
			mtlsAddr:      ":8443",
			want:          "https://mtls.example.com:9443",
			wantOK:        true,
		},
		{
			name:          "fallback to panel host and mtls port",
			panelPublic:   "https://proxy.example.com",
			mtlsPublicURL: "",
			mtlsAddr:      ":9443",
			want:          "https://proxy.example.com:9443",
			wantOK:        true,
		},
		{
			name:          "invalid explicit mtls url falls back",
			panelPublic:   "https://proxy.example.com",
			mtlsPublicURL: "://bad-url",
			mtlsAddr:      ":8443",
			want:          "https://proxy.example.com:8443",
			wantOK:        true,
		},
		{
			name:          "invalid panel public url fails closed",
			panelPublic:   "bad-url",
			mtlsPublicURL: "",
			mtlsAddr:      ":8443",
			want:          "",
			wantOK:        false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := derivePanelMTLSURL(tc.panelPublic, tc.mtlsPublicURL, tc.mtlsAddr)
			if ok != tc.wantOK {
				t.Fatalf("derivePanelMTLSURL() ok=%t, want %t", ok, tc.wantOK)
			}
			if got != tc.want {
				t.Fatalf("derivePanelMTLSURL()=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildNodeDeployScriptIncludesDockerBootstrap(t *testing.T) {
	script := buildNodeDeployScript(
		"/root/neutrino-node",
		"https://panel.example.com",
		"https://mtls.example.com:8443",
		12,
		"code-123",
		"example/agent:1",
		"example/xray:1",
		24443,
	)

	checks := []string{
		"DOCKER_COMPOSE_VERSION=${DOCKER_COMPOSE_VERSION:-v5.0.1}",
		"ensure_docker()",
		"install_docker_engine()",
		"install_docker_engine_apt()",
		"apt_update()",
		"repair_debian_bullseye_apt_sources()",
		"bullseye-security",
		"bullseye-backports",
		"info: docker not found, trying to install Docker Engine...",
		"https://download.docker.com/linux/${os_id}",
		"apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin",
		"apt-get install -y docker.io docker-compose-plugin",
		"docker daemon is not running; start docker and rerun this script",
		"ensure_docker_compose()",
		"install_docker_compose_binary()",
		"apt-get install -y docker-compose-plugin || true",
		"dnf install -y docker-compose-plugin || true",
		"yum install -y docker-compose-plugin || true",
		"apk add --no-cache docker-cli-compose || true",
		"https://github.com/docker/compose/releases/download/${DOCKER_COMPOSE_VERSION}/docker-compose-linux-${compose_arch}",
		"HOSTNET_ENABLE=true",
		"XRAY_API_LISTEN=127.0.0.1",
		"AGENT_HTTP_ADDR=127.0.0.1:9090",
		"XRAY_API_ADDR=127.0.0.1:10085",
		"AGENT_ACCESS_LOG_TZ=UTC",
		"network_mode: host",
		"compose_cmd up -d",
	}

	for _, check := range checks {
		if !strings.Contains(script, check) {
			t.Fatalf("expected deploy script to contain %q", check)
		}
	}
	if strings.Contains(script, "docker is required before installing docker compose") {
		t.Fatal("deploy script still exits before installing Docker Engine")
	}
}

func TestBuildNodeDeployScriptShellSyntax(t *testing.T) {
	script := buildNodeDeployScript(
		"/root/neutrino-node",
		"https://panel.example.com",
		"https://mtls.example.com:8443",
		12,
		"code-123",
		"example/agent:1",
		"example/xray:1",
		24443,
	)
	path := t.TempDir() + "/deploy.sh"
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatalf("write script: %v", err)
	}
	out, err := exec.Command("sh", "-n", path).CombinedOutput()
	if err != nil {
		t.Fatalf("generated deploy script has invalid shell syntax: %v\n%s", err, string(out))
	}
}
