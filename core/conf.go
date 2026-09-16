package core

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The breez library reads breez.conf and lnd.conf from the working dir.
// These templates follow the production files shipped inside the mobile
// APK (assets/flutter_assets/conf), minus bug reporting and Tor.
//
// With no peers given, neutrino discovers compact-filter peers through the
// Bitcoin DNS seeds instead of pinning the Breez btcd hosts, so the tool
// keeps working after those hosts go away.
func (c *Core) writeConfigs() error {
	if err := os.MkdirAll(c.cfg.WorkDir, 0700); err != nil {
		return err
	}
	breezConf := filepath.Join(c.cfg.WorkDir, "breez.conf")
	lndConf := filepath.Join(c.cfg.WorkDir, "lnd.conf")

	jobPeer := ""
	neutrinoConnect := ""
	if strings.TrimSpace(c.cfg.Peers) == "" {
		// Discovery through the DNS seeds stays on, but the Breez node is
		// added as a known good compact-filter peer while it exists: the
		// public peers found through the seeds are often slow or drop
		// the long filter queries a wallet rescan needs.
		for _, p := range DefaultExtraPeers {
			neutrinoConnect += "neutrino.addpeer=" + p + "\n"
		}
	}
	for _, p := range strings.Split(c.cfg.Peers, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		jobPeer += "peer=" + p + "\n"
		neutrinoConnect += "neutrino.connect=" + p + "\n"
	}
	lspTokenLine := ""
	if c.cfg.LSPToken != "" {
		lspTokenLine = "lsptoken=" + c.cfg.LSPToken + "\n"
	}

	breez := fmt.Sprintf(`[Application Options]
network=%s
breezserver=%s
bootstrap=%s
closedchannelsurl=%s
grpckeepalive=0
%s[Job Options]
%s`, c.cfg.Network, c.cfg.BreezServer, c.cfg.BootstrapURL, c.cfg.ClosedChannelsURL, lspTokenLine, jobPeer)

	lnd := fmt.Sprintf(`[Application Options]
debuglevel=info
noseedbackup=1
nolisten=1
rpcmemlisten=1
nobootstrap=1
maxbackoff=20s
payments-expiration-grace-period=24h
initial-headers-sync-delta=2h
[protocol]
protocol.option-scid-alias=true
protocol.zero-conf=true
[Bitcoin]
bitcoin.active=1
bitcoin.%s=1
bitcoin.node=neutrino
bitcoin.defaultchanconfs=1
bitcoin.defaultremotedelay=720
[Routing]
routing.assumechanvalid=1
[fee]
fee.url=%s
[Neutrino]
%s`, c.cfg.Network, c.cfg.FeeURL, neutrinoConnect)

	if err := os.WriteFile(breezConf, []byte(breez), 0600); err != nil {
		return err
	}
	return os.WriteFile(lndConf, []byte(lnd), 0600)
}
