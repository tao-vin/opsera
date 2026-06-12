package model

type ConnectionMode string

type MachineType string

const (
	ConnectionModeDirectSSH   ConnectionMode = "direct_ssh"
	ConnectionModeVPNLauncher ConnectionMode = "vpn_launcher"
	ConnectionModeLocalTunnel ConnectionMode = "local_tunnel"
	ConnectionModeRelayAgent  ConnectionMode = "relay_agent"
)

const (
	MachineTypeLinux   MachineType = "linux"
	MachineTypeWindows MachineType = "windows"
)

type Server struct {
	ID                string         `json:"id"`
	Name              string         `json:"name"`
	Host              string         `json:"host"`
	Port              int            `json:"port"`
	Mode              ConnectionMode `json:"mode"`
	MachineType       MachineType    `json:"machineType,omitempty"`
	MachineCode       string         `json:"machineCode,omitempty"`
	CredentialRef     string         `json:"credentialRef"`
	LocalTunnelHost   string         `json:"localTunnelHost,omitempty"`
	LocalTunnelPort   int            `json:"localTunnelPort,omitempty"`
	SecureCRTSession  string         `json:"secureCrtSession,omitempty"`
	WinSCPTunnelPort  int            `json:"winScpTunnelPort,omitempty"`
	Tags              []string       `json:"tags,omitempty"`
	HealthcheckTarget string         `json:"healthcheckTarget,omitempty"`
}
