// Command ampiocli keeps an Ampio installation's app configuration, module
// logic and Node-RED flows as a folder of JSON files: pull, edit, diff, push.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"

	"github.com/daisied/ampiocli/internal/config"
	"github.com/daisied/ampiocli/internal/designer"
	"github.com/daisied/ampiocli/internal/engine"
	"github.com/daisied/ampiocli/internal/logic"
	"github.com/daisied/ampiocli/internal/mserv"
	"github.com/daisied/ampiocli/internal/nodered"
	"github.com/daisied/ampiocli/internal/sim"
	"github.com/daisied/ampiocli/internal/workdir"
)

const usage = `usage: ampiocli [-C dir] <command> [flags]

commands:
  pull [--force]   read the M-SERV into the working copy (--force discards unpushed edits)
  diff             show the edits not pushed yet (offline)
  push [--yes]     show what push would write; with --yes, write it and verify
  backups          list the backups push made
  restore NAME [--force]
                   put a backup into the working copy; push then writes it back
                   (--force discards unpushed edits)
  sim              serve the last pull as a stand-in M-SERV on this computer,
                   to try edits on a copy of the installation

  pull, diff and push take --only config,logic,nodered to limit the domains.

settings, from the environment or <dir>/.env:
  AMPIO_HOST, AMPIO_PASSWORD   the M-SERV and its admin password
  AMPIO_USER                   default admin
  AMPIO_MQTT_URL, AMPIO_DESIGNER_URL, AMPIO_NODERED_URL
                               override the addresses derived from AMPIO_HOST
  NODERED_USER, NODERED_PASSWORD
                               default to the admin login
`

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "ampiocli:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	global := flag.NewFlagSet("ampiocli", flag.ContinueOnError)
	global.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	dir := global.String("C", ".", "")
	if err := global.Parse(args); err != nil {
		return help(err)
	}
	if global.NArg() == 0 {
		global.Usage()
		return errors.New("no command")
	}
	cmd := global.Arg(0)
	if !slices.Contains([]string{"pull", "diff", "push", "backups", "restore", "sim"}, cmd) {
		global.Usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
	flags := flag.NewFlagSet(cmd, flag.ContinueOnError)
	flags.Usage = global.Usage
	var only string
	var force, yes bool
	switch cmd {
	case "pull", "restore":
		flags.BoolVar(&force, "force", false, "")
	case "push":
		flags.BoolVar(&yes, "yes", false, "")
	}
	if cmd == "pull" || cmd == "diff" || cmd == "push" {
		flags.StringVar(&only, "only", "", "")
	}
	// Flags may come before or after the arguments: restore NAME --force.
	var params []string
	for rest := global.Args()[1:]; ; {
		if err := flags.Parse(rest); err != nil {
			return help(err)
		}
		if flags.NArg() == 0 {
			break
		}
		params, rest = append(params, flags.Arg(0)), flags.Args()[1:]
	}
	if cmd != "restore" && len(params) > 0 {
		return fmt.Errorf("%s takes no arguments, got %q", cmd, params[0])
	}

	settings, err := mserv.LoadSettings(*dir)
	if err != nil {
		return err
	}
	conn := mserv.New(settings)
	defer conn.Close()
	domains := []engine.Domain{config.New(conn), logic.New(conn, settings.Designer, out), nodered.New(settings)}
	if only != "" {
		if domains, err = pick(domains, strings.Split(only, ",")); err != nil {
			return err
		}
	}
	e := &engine.Engine{Copy: workdir.Copy(*dir), Domains: domains, Out: out}

	// The first Ctrl-C stops the work cleanly; a second one ends the process.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	context.AfterFunc(ctx, stop)
	switch cmd {
	case "pull":
		return e.Pull(ctx, force)
	case "diff":
		return e.Diff()
	case "push":
		return e.Push(ctx, yes)
	case "backups":
		names, err := e.Copy.Backups()
		for _, n := range names {
			fmt.Fprintln(out, n)
		}
		return err
	case "restore":
		if len(params) != 1 {
			return errors.New("restore takes a backup name (see ampiocli backups)")
		}
		return e.Restore(params[0], force)
	default: // sim
		return serve(ctx, e, settings.Designer)
	}
}

// serve runs the simulator until interrupted. It serves Designer from the
// cache a pull from designerURL filled.
func serve(ctx context.Context, e *engine.Engine, designerURL string) error {
	files, err := e.Copy.Base().Read(workdir.All)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return errors.New("nothing pulled yet")
	}
	cache, err := designer.CacheDir(designerURL)
	if err != nil {
		return err
	}
	s, err := sim.Start(files, cache, e.Out)
	if err != nil {
		return err
	}
	defer s.Close()
	fmt.Fprintf(e.Out, "stand-in M-SERV running; in another folder's .env (or the environment) set:\n"+
		"AMPIO_MQTT_URL=%s\nAMPIO_DESIGNER_URL=%s\nAMPIO_NODERED_URL=%s\nAMPIO_PASSWORD=%s\n\nCtrl-C stops it.\n",
		s.Settings.MQTT, s.Settings.Designer, s.Settings.NodeRED, s.Settings.Password)
	<-ctx.Done()
	return nil
}

// help turns -h, which has printed the usage, into success.
func help(err error) error {
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}

// pick keeps the named domains, in write order.
func pick(domains []engine.Domain, names []string) ([]engine.Domain, error) {
	for _, n := range names {
		if !slices.ContainsFunc(domains, func(d engine.Domain) bool { return d.Name() == n }) {
			return nil, fmt.Errorf("--only: no domain %q (there are config, logic and nodered)", n)
		}
	}
	return slices.DeleteFunc(domains, func(d engine.Domain) bool { return !slices.Contains(names, d.Name()) }), nil
}
