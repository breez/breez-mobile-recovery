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
// Bitcoin peers go into breez.conf [Job Options] as peer= lines: the
// library builds neutrino itself from that list (chainservice/init.go) and
// ignores lnd.conf's [Neutrino] section. The list is exclusive: neutrino
// connects only to those hosts and skips DNS seed discovery. That is how
// the mobile app ran, and it matters: peers found through the seeds are
// slow or drop the long filter queries a wallet rescan needs (76 blocks/s
// against 600/s on the Breez node).
func (c *Core) writeConfigs() error {
	// Everything below is interpolated into ini files; a value with a line
	// break or spaces could inject settings, so refuse those.
	for name, v := range map[string]string{"peers": c.cfg.Peers, "lsp token": c.cfg.LSPToken, "breez server": c.cfg.BreezServer, "bootstrap url": c.cfg.BootstrapURL, "closed channels url": c.cfg.ClosedChannelsURL, "fee url": c.cfg.FeeURL, "network": c.cfg.Network, "log level": c.cfg.LogLevel} {
		if strings.ContainsAny(v, "\r\n\t []") {
			return fmt.Errorf("invalid %s: must not contain spaces, brackets or line breaks", name)
		}
	}
	if err := os.MkdirAll(c.cfg.WorkDir, 0700); err != nil {
		return err
	}
	breezConf := filepath.Join(c.cfg.WorkDir, "breez.conf")
	lndConf := filepath.Join(c.cfg.WorkDir, "lnd.conf")

	jobPeer := ""
	for _, p := range c.peers() {
		jobPeer += "peer=" + p + "\n"
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

	logLevel := c.cfg.LogLevel
	if logLevel == "" {
		logLevel = "info"
	}
	lnd := fmt.Sprintf(`[Application Options]
debuglevel=%s
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
`, logLevel, c.cfg.Network, c.cfg.FeeURL)

	if err := os.WriteFile(breezConf, []byte(breez), 0600); err != nil {
		return err
	}
	return os.WriteFile(lndConf, []byte(lnd), 0600)
}

// peers is the pinned bitcoin peer list: the configured one, else the
// Breez node.
func (c *Core) peers() []string {
	var out []string
	for _, p := range strings.Split(c.cfg.Peers, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		out = append(out, DefaultPeers...)
	}
	return out
}
