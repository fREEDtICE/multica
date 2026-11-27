package provision

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	runtimeConfigPath = "/etc/helios/runtime.json"
	leasePath         = "/etc/helios/lease.json"
	agentBinary       = "/opt/helios/bin/helios-runtime-agent"
	// RegisterPath is the existing formal Multica daemon registration entry.
	RegisterPath = "/api/daemon/register"
)

type runtimeConfig struct {
	WorkspaceURL    string `json:"workspace_url"`
	RegisterPath    string `json:"register_path"`
	DaemonID        string `json:"daemon_id"`
	RuntimeName     string `json:"runtime_name"`
	RuntimeType     string `json:"runtime_type"`
	RuntimeMode     string `json:"runtime_mode"`
	Provider        string `json:"provider"`
	Region          string `json:"region"`
	InstanceProfile string `json:"instance_profile"`
	ModelGatewayURL string `json:"model_gateway_url"`
	LeasePath       string `json:"lease_path"`
}

type leaseConfig struct {
	Lease        string   `json:"lease"`
	ExpiresAt    int64    `json:"expires_at"`
	Capabilities []string `json:"capabilities"`
}

// BootstrapInput carries the values rendered into first-boot user-data.
type BootstrapInput struct {
	WorkspaceURL    string
	Provider        string
	Region          string
	RuntimeName     string
	DaemonID        string
	InstanceProfile string
	GatewayURL      string
	Lease           string
	LeaseExpiresAt  int64
}

// RenderUserData returns a cloud-init compatible shell script executed on the
// instance's first boot. It only references the instance profile and embeds a
// short-lived task lease; long-lived cloud management keys are never rendered.
func RenderUserData(in BootstrapInput) (string, error) {
	if in.LeaseExpiresAt == 0 || len(in.Lease) == 0 {
		return "", &DriverError{Code: ErrConfiguration, Message: "bootstrap requires a short-lived lease"}
	}
	rc := runtimeConfig{
		WorkspaceURL:    in.WorkspaceURL,
		RegisterPath:    RegisterPath,
		DaemonID:        in.DaemonID,
		RuntimeName:     in.RuntimeName,
		RuntimeType:     "cloud-" + in.Provider,
		RuntimeMode:     "cloud",
		Provider:        in.Provider,
		Region:          in.Region,
		InstanceProfile: in.InstanceProfile,
		ModelGatewayURL: in.GatewayURL,
		LeasePath:       leasePath,
	}
	lc := leaseConfig{
		Lease:        in.Lease,
		ExpiresAt:    in.LeaseExpiresAt,
		Capabilities: []string{"source.save", "model.invoke", "build.read"},
	}
	rcJSON, err := json.MarshalIndent(rc, "", "  ")
	if err != nil {
		return "", err
	}
	lcJSON, err := json.MarshalIndent(lc, "", "  ")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`#!/bin/bash
set -euo pipefail
# Rendered by the Multica built-in cloud runtime. Executed once by the cloud
# first-boot mechanism; no SSH login or manual executor registration.
mkdir -p /etc/helios /opt/helios/bin
cat > %s <<'HELIOS_RUNTIME_JSON'
%s
HELIOS_RUNTIME_JSON
cat > %s <<'HELIOS_LEASE_JSON'
%s
HELIOS_LEASE_JSON
chmod 600 %s
exec %s --config %s
`, runtimeConfigPath, rcJSON, leasePath, lcJSON, leasePath, agentBinary, runtimeConfigPath), nil
}

// embeddedFiles extracts the heredoc payloads from rendered user-data. RE2 has
// no backreferences, so the start/end markers are matched line by line.
func embeddedFiles(userData string) map[string]string {
	files := map[string]string{}
	lines := strings.Split(userData, "\n")
	for i := 0; i < len(lines); i++ {
		path, marker, ok := parseHeredocStart(lines[i])
		if !ok {
			continue
		}
		var body []string
		for i++; i < len(lines) && lines[i] != marker; i++ {
			body = append(body, lines[i])
		}
		files[path] = strings.Join(body, "\n")
	}
	return files
}

// parseHeredocStart reads a line shaped: cat > /path <<'MARKER'
func parseHeredocStart(line string) (path, marker string, ok bool) {
	const prefix = "cat > "
	if !strings.HasPrefix(line, prefix) || !strings.Contains(line, "<<'") {
		return "", "", false
	}
	rest := strings.TrimPrefix(line, prefix)
	parts := strings.SplitN(rest, " <<'", 2)
	if len(parts) != 2 || !strings.HasSuffix(parts[1], "'") {
		return "", "", false
	}
	return parts[0], strings.TrimSuffix(parts[1], "'"), true
}

// ParseLease returns the lease document embedded in rendered user-data.
func ParseLease(userData string) (leaseConfig, error) {
	var lease leaseConfig
	err := json.Unmarshal([]byte(embeddedFiles(userData)[leasePath]), &lease)
	return lease, err
}

// ParseRuntimeConfig returns the runtime configuration embedded in user-data.
func ParseRuntimeConfig(userData string) (runtimeConfig, error) {
	var cfg runtimeConfig
	err := json.Unmarshal([]byte(embeddedFiles(userData)[runtimeConfigPath]), &cfg)
	return cfg, err
}
