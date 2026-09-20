// Command recovery is the command line front of the Breez mobile recovery
// tool. The desktop app in ./ui is the same thing with a window. Both are
// thin layers over ./core.
//
// Typical flow:
//
//	recovery snapshots                         # sign in to Google, list backups
//	recovery restore --node-id <id> --mnemonic "word1 ... word24"
//	recovery status                            # start node, wait for sync, print balances
//	recovery sweep --address bc1...            # send the on-chain balance
//
// iCloud backups: add --icloud to snapshots and restore. A backup zip on
// disk: recovery restore --zip backup.zip --mnemonic "...".
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/breez/breez-mobile-recovery/core"
)

var (
	cfg     = core.DefaultConfig()
	verbose bool
	out     = os.Stdout // the real stdout; core captures os.Stdout for library logs
)

// cliReporter prints progress to stdout and, with -v, node logs to stderr.
type cliReporter struct{}

func (cliReporter) Progress(msg string) { fmt.Fprintln(out, msg) }
func (cliReporter) NodeLog(line string) {
	if verbose {
		fmt.Fprintln(os.Stderr, line)
	}
}
func (cliReporter) SignIn(provider, url string) {
	fmt.Fprintln(out, "If the browser did not open, visit this URL:")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  "+url)
	fmt.Fprintln(out)
}

func usage() {
	fmt.Fprintf(os.Stderr, `Breez mobile recovery tool

Usage:
  recovery [global flags] <command> [command flags]

Commands:
  snapshots   List node backups in Google Drive (or iCloud with --icloud)
  restore     Download (Drive, --icloud, or --zip file) a backup into a folder of its own
  backups     List the backups restored on this computer
  use         Continue with another restored backup: recovery use <name>
  status      Start the node, wait for chain sync and print balances and channels
  sweep       Send the whole on-chain wallet balance to --address
  history     Every payment sent or received, channel closes and on-chain moves, with totals
  lncli       Run an lncli command against the restored node (escape hatch)

Global flags:
`)
	flag.PrintDefaults()
	fmt.Fprintf(os.Stderr, "\nRun 'recovery <command> -h' for command flags.\n")
}

func main() {
	flag.StringVar(&cfg.WorkDir, "workdir", cfg.WorkDir, "work folder; every restored backup gets a folder of its own under it")
	flag.StringVar(&cfg.Network, "network", cfg.Network, "bitcoin network (mainnet, testnet, simnet)")
	flag.StringVar(&cfg.BreezServer, "breezserver", cfg.BreezServer, "Breez server address used by the node for LSP services")
	flag.StringVar(&cfg.BootstrapURL, "bootstrap", cfg.BootstrapURL, "Breez bootstrap URL")
	flag.StringVar(&cfg.ClosedChannelsURL, "closedchannelsurl", cfg.ClosedChannelsURL, "Breez closed channels service URL")
	flag.StringVar(&cfg.LSPToken, "lsptoken", cfg.LSPToken, "Breez LSP token (optional)")
	flag.StringVar(&cfg.FeeURL, "feeurl", cfg.FeeURL, "fee estimator URL lnd uses with neutrino")
	flag.StringVar(&cfg.ICloudAPIToken, "icloud-token", cfg.ICloudAPIToken, "CloudKit API token for iCloud backups")
	flag.StringVar(&cfg.Peers, "peer", "", "comma-separated bitcoin peers with compact filters; empty = the Breez nodes")
	flag.StringVar(&cfg.LogLevel, "loglevel", cfg.LogLevel, "lnd log level, for support: info, debug, or per subsystem like NTFN=debug,CNCT=debug,BTCN=debug")
	flag.BoolVar(&verbose, "v", false, "forward node logs and notifications to stderr")
	flag.Usage = usage
	flag.Parse()

	if flag.NArg() < 1 {
		usage()
		os.Exit(2)
	}
	cmd, args := flag.Arg(0), flag.Args()[1:]

	c := core.New(cfg, cliReporter{})
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var err error
	switch cmd {
	case "snapshots":
		err = cmdSnapshots(ctx, c, args)
	case "restore":
		err = cmdRestore(ctx, c, args)
	case "backups":
		for _, b := range c.RestoredBackups() {
			mark := " "
			if b.Current {
				mark = "*"
			}
			fmt.Fprintf(out, "%s %s  %s\n", mark, b.Name, b.Dir)
		}
	case "use":
		if len(args) != 1 {
			err = errors.New("usage: recovery use <name from 'recovery backups'>")
			break
		}
		err = c.UseBackup(args[0])
	case "status":
		err = cmdStatus(ctx, c, args)
	case "sweep":
		err = cmdSweep(ctx, c, args)
	case "history":
		err = cmdHistory(ctx, c, args)
	case "lncli":
		err = cmdLncli(ctx, c, args)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	c.Stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// ---- snapshots ------------------------------------------------------------

func cmdSnapshots(ctx context.Context, c *core.Core, args []string) error {
	fs := flag.NewFlagSet("snapshots", flag.ExitOnError)
	icloud := fs.Bool("icloud", false, "list iCloud backups instead of Google Drive")
	fs.Parse(args)

	var snaps []core.Snapshot
	var err error
	if *icloud {
		snaps, err = c.ICloudSnapshots(ctx)
	} else {
		snaps, err = c.GoogleSnapshots(ctx)
	}
	if err != nil {
		return err
	}
	printSnapshots(snaps)
	return nil
}

func printSnapshots(snaps []core.Snapshot) {
	fmt.Fprintf(out, "%-66s  %-20s  %s\n", "NODE ID", "LAST BACKUP", "ENCRYPTION")
	for _, s := range snaps {
		enc := "none"
		switch s.EncryptionType {
		case "Mnemonics":
			enc = "24-word phrase"
		case "Mnemonics12":
			enc = "12-word phrase"
		case "PIN":
			enc = "legacy PIN (unsupported)"
		}
		fmt.Fprintf(out, "%-66s  %-20s  %s\n", s.NodeID, s.ModifiedTime.Local().Format("2006-01-02 15:04"), enc)
	}
}

// ---- restore --------------------------------------------------------------

func cmdRestore(ctx context.Context, c *core.Core, args []string) error {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	nodeID := fs.String("node-id", "", "node id from 'recovery snapshots' (omit when only one backup exists)")
	zipPath := fs.String("zip", "", "restore from a local backup zip instead of the cloud")
	icloud := fs.Bool("icloud", false, "restore from iCloud instead of Google Drive")
	mnemonic := fs.String("mnemonic", "", "backup phrase (12 or 24 words); prompted if omitted and needed")
	force := fs.Bool("force", false, "restore a backup again that is already restored here; the earlier copy is moved aside")
	fs.Parse(args)

	var err error
	switch {
	case *zipPath != "":
		phrase := *mnemonic
		if phrase == "" {
			needs, err := core.ZipNeedsPhrase(*zipPath)
			if err != nil {
				return err
			}
			if needs {
				phrase = promptPhrase()
			}
		}
		err = c.ZipRestore(*zipPath, phrase, *force)
	case *icloud:
		var snaps []core.Snapshot
		if snaps, err = c.ICloudSnapshots(ctx); err != nil {
			return err
		}
		snap, perr := pick(snaps, *nodeID)
		if perr != nil {
			return perr
		}
		err = c.ICloudRestore(ctx, snap.NodeID, phraseFor(snap, *mnemonic), *force)
	default:
		var snaps []core.Snapshot
		if snaps, err = c.GoogleSnapshots(ctx); err != nil {
			return err
		}
		snap, perr := pick(snaps, *nodeID)
		if perr != nil {
			return perr
		}
		err = c.GoogleRestore(ctx, snap.NodeID, phraseFor(snap, *mnemonic), *force)
	}
	if errors.Is(err, core.ErrNodeExists) {
		return fmt.Errorf("%w; 'recovery use <name>' continues with it, --force restores it again", err)
	}
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "Next: 'recovery status' to sync and see balances, then 'recovery sweep --address <btc address>'.")
	return nil
}

func pick(snaps []core.Snapshot, nodeID string) (core.Snapshot, error) {
	if nodeID == "" && len(snaps) == 1 {
		return snaps[0], nil
	}
	for _, s := range snaps {
		if s.NodeID == nodeID {
			return s, nil
		}
	}
	printSnapshots(snaps)
	if nodeID == "" {
		return core.Snapshot{}, errors.New("several backups found, pick one with --node-id")
	}
	return core.Snapshot{}, fmt.Errorf("node id %s not found among the backups above", nodeID)
}

func phraseFor(snap core.Snapshot, mnemonic string) string {
	if snap.Encrypted && mnemonic == "" {
		return promptPhrase()
	}
	return mnemonic
}

func promptPhrase() string {
	fmt.Fprint(out, "Enter your backup phrase: ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(line)
}

// ---- status ---------------------------------------------------------------

func startAndSync(ctx context.Context, c *core.Core) error {
	if err := c.StartNode(ctx); err != nil {
		if errors.Is(err, core.ErrRestartRequired) {
			fmt.Fprintln(out, "The node was prepared in this run. Run the same command again to continue.")
			os.Exit(0) // not c.Stop(): it may hang, and the exit releases everything
		}
		return err
	}
	if err := c.WaitSynced(ctx, func(p core.SyncProgress) { fmt.Fprintln(out, p.Message) }); err != nil {
		return err
	}
	_, err := c.CheckChannelsOnChain(ctx, func(p core.SyncProgress) {
		fmt.Fprintf(out, "%s, block %d of %d\n", p.Message, p.Height, p.Target)
	})
	return err
}

func cmdStatus(ctx context.Context, c *core.Core, args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	fs.Parse(args)

	if err := startAndSync(ctx, c); err != nil {
		return err
	}
	st, err := c.Status(ctx)
	if err != nil {
		return err
	}
	printStatus(st)
	return nil
}

func printStatus(st *core.Status) {
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Node:            %s\n", st.NodeID)
	fmt.Fprintf(out, "Block height:    %d (synced)\n", st.BlockHeight)
	fmt.Fprintf(out, "Peers:           %d\n", st.Peers)
	fmt.Fprintf(out, "On-chain:        %d sat confirmed, %d sat unconfirmed\n", st.OnchainConfirmed, st.OnchainUnconfirmed)
	fmt.Fprintf(out, "Open channels:   %d\n", len(st.Channels))
	if len(st.Channels) > 0 {
		fmt.Fprintln(out, "  These channels are still open. Email this output to Breez support: contact@breez.technology.")
	}
	for _, c := range st.Channels {
		state := "active"
		if !c.Active {
			state = "inactive (peer offline)"
		}
		if c.Dust {
			state += fmt.Sprintf(", below its dust limit of %d sat: not counted", c.DustLimit)
		}
		fmt.Fprintf(out, "  %s  peer %s  local %d sat  remote %d sat  %s\n", c.ChannelPoint, c.Peer, c.LocalBalance, c.RemoteBalance, state)
	}
	for _, c := range st.ClosedOnChain {
		line := fmt.Sprintf("  %s  closed on chain in %s", c.ChannelPoint, c.ClosingTxID)
		if c.Collect > 0 {
			line += fmt.Sprintf(", %d sat being collected", c.Collect)
		}
		fmt.Fprintln(out, line)
	}
	fmt.Fprintf(out, "Pending closes:  %d\n", len(st.Pending))
	for _, p := range st.Pending {
		switch p.Kind {
		case "waiting":
			fmt.Fprintf(out, "  %s  waiting for close tx to confirm, %d sat in limbo\n", p.ChannelPoint, p.Amount)
		case "cooperative":
			fmt.Fprintf(out, "  %s  cooperative close %s confirming, %d sat\n", p.ChannelPoint, p.ClosingTxID, p.Amount)
		case "force":
			fmt.Fprintf(out, "  %s  force close %s, %d sat in limbo, %d blocks until spendable\n", p.ChannelPoint, p.ClosingTxID, p.Amount, p.BlocksToMature)
		}
	}
	for _, w := range st.Warnings {
		fmt.Fprintf(out, "Warning:         %s\n", w)
	}
	fmt.Fprintln(out)
}

// ---- sweep ----------------------------------------------------------------

func cmdSweep(ctx context.Context, c *core.Core, args []string) error {
	fs := flag.NewFlagSet("sweep", flag.ExitOnError)
	address := fs.String("address", "", "bitcoin address that receives the on-chain balance")
	target := fs.Int("conf-target", 6, "fee target in blocks (2, 6 or 25)")
	yes := fs.Bool("yes", false, "broadcast without asking for confirmation")
	fs.Parse(args)
	if *address == "" {
		return errors.New("--address is required")
	}
	if err := core.ValidateAddress(*address); err != nil {
		return fmt.Errorf("invalid address: %w", err)
	}
	if err := startAndSync(ctx, c); err != nil {
		return err
	}
	plan, err := c.PrepareSweep(ctx, *address)
	if err != nil {
		return err
	}
	var chosen *core.SweepOption
	for i := range plan.Options {
		if plan.Options[i].ConfTarget == *target {
			chosen = &plan.Options[i]
		}
	}
	if chosen == nil {
		return fmt.Errorf("no transaction prepared for conf target %d", *target)
	}
	fmt.Fprintf(out, "Sweep %d sat to %s, fee %d sat (target %d blocks), txid %s\n", plan.Amount, *address, chosen.Fee, chosen.ConfTarget, chosen.TxID)
	if !*yes {
		fmt.Fprint(out, "Broadcast? [y/N] ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if l := strings.ToLower(strings.TrimSpace(line)); l != "y" && l != "yes" {
			return errors.New("aborted")
		}
	}
	txid, err := c.BroadcastSweep(chosen.ConfTarget)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Broadcast %s\n", txid)
	return nil
}

// ---- history --------------------------------------------------------------

func cmdHistory(ctx context.Context, c *core.Core, args []string) error {
	fs := flag.NewFlagSet("history", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the ledger and the app's raw payment list as JSON")
	asCSV := fs.Bool("csv", false, "print the ledger as CSV (a spreadsheet)")
	fs.Parse(args)
	if err := startAndSync(ctx, c); err != nil {
		return err
	}
	h, err := c.History(ctx)
	if err != nil {
		return err
	}
	if *asCSV {
		return core.WriteHistoryCSV(out, h)
	}
	if *asJSON {
		payments, err := c.RawPayments()
		if err != nil {
			return err
		}
		lnd, err := c.LndTotals(ctx, payments)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]interface{}{"history": h, "payments": payments, "lnd": lnd})
	}
	fmt.Fprintln(out)
	for _, w := range h.Warnings {
		fmt.Fprintf(out, "Warning: %s\n", w)
	}
	fmt.Fprintf(out, "%-17s %-24s %14s %10s  %s\n", "DATE", "WHAT", "AMOUNT", "FEE", "DETAILS")
	for _, e := range h.Entries {
		when := "unknown date"
		if e.Time > 0 {
			when = time.Unix(e.Time, 0).Local().Format("2006-01-02 15:04")
		}
		amount := fmt.Sprintf("%d sat", e.Amount)
		switch {
		case e.Delta > 0:
			amount = "+" + amount
		case e.Delta < 0:
			amount = "-" + amount
		}
		fee := ""
		if e.Fee > 0 {
			fee = fmt.Sprintf("%d sat", e.Fee)
		}
		what := e.Title
		if e.Status != "done" {
			what += " (" + e.Status + ")"
		}
		fmt.Fprintf(out, "%-17s %-24s %14s %10s  %s\n", when, what, amount, fee, e.Detail)
		if e.FeeNote != "" {
			fmt.Fprintf(out, "%-17s %-24s %14s %10s  fee %s\n", "", "", "", "", e.FeeNote)
		}
		if e.Note != "" {
			fmt.Fprintf(out, "%-17s %-24s %14s %10s  %s\n", "", "", "", "", e.Note)
		}
		if e.TxID != "" {
			fmt.Fprintf(out, "%-17s %-24s %14s %10s  tx %s\n", "", "", "", "", e.TxID)
		}
	}
	if h.ZeroCloses > 0 {
		fmt.Fprintf(out, "\n%d channel(s) closed with no balance of yours are not listed.\n", h.ZeroCloses)
	}
	t := h.Totals
	fmt.Fprintf(out, "\nIn        %14d sat\n", t.In)
	fmt.Fprintf(out, "Out       %14d sat\n", t.Out)
	fmt.Fprintf(out, "Fees      %14d sat\n", t.Fees)
	fmt.Fprintf(out, "Expected  %14d sat  (in - out - fees)\n", t.Expected)
	fmt.Fprintf(out, "Held now  %14d sat  (on-chain %d, in channels %d, closing %d)\n", t.Held, t.Onchain, t.InChannels, t.InPending)
	if t.Uncollected != 0 {
		fmt.Fprintf(out, "Set aside %14d sat  by channel closes, not collected into the on-chain balance yet\n", t.Uncollected)
	}
	fmt.Fprintln(out)
	return nil
}

// ---- lncli ----------------------------------------------------------------

func cmdLncli(ctx context.Context, c *core.Core, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: recovery lncli <command> [flags]")
	}
	if err := c.StartNode(ctx); err != nil {
		return err
	}
	res, err := c.Lncli(ctx, strings.Join(args, " "))
	if err != nil {
		return err
	}
	fmt.Fprintln(out, res)
	return nil
}
