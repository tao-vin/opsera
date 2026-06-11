package model

type ConnectionMode string

const (
	ConnectionModeDirectSSH   ConnectionMode = "direct_ssh"
	ConnectionModeVPNLauncher ConnectionMode = "vpn_launcher"
	ConnectionModeLocalTunnel ConnectionMode = "local_tunnel"
)

type Server struct {
	ID                string         `json:"id"`
	Name              string         `json:"name"`
	Host              string         `json:"host"`
	Port              int            `json:"port"`
	Mode              ConnectionMode `json:"mode"`
	CredentialRef     string         `json:"credentialRef"`
	LocalTunnelHost   string         `json:"localTunnelHost,omitempty"`
	LocalTunnelPort   int            `json:"localTunnelPort,omitempty"`
	SecureCRTSession  string         `json:"secureCrtSession,omitempty"`
	WinSCPTunnelPort  int            `json:"winScpTunnelPort,omitempty"`
	Tags              []string       `json:"tags,omitempty"`
	HealthcheckTarget string         `json:"healthcheckTarget,omitempty"`
}
