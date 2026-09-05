package channel

import "sipclient/internal/config"

// testConfig is a minimal valid configuration for manager unit tests.
func testConfig() config.Config {
	cfg := config.Default()
	cfg.SIP.Username = "1001"
	cfg.SIP.Password = "pw"
	cfg.SIP.Domain = "test"
	cfg.SIP.Server.Host = "127.0.0.1"
	return cfg
}
