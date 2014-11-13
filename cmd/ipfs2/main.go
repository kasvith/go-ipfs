package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/pprof"

	logging "github.com/jbenet/go-ipfs/Godeps/_workspace/src/github.com/jbenet/go-logging"
	ma "github.com/jbenet/go-ipfs/Godeps/_workspace/src/github.com/jbenet/go-multiaddr"
	manet "github.com/jbenet/go-ipfs/Godeps/_workspace/src/github.com/jbenet/go-multiaddr/net"

	cmds "github.com/jbenet/go-ipfs/commands"
	cmdsCli "github.com/jbenet/go-ipfs/commands/cli"
	cmdsHttp "github.com/jbenet/go-ipfs/commands/http"
	"github.com/jbenet/go-ipfs/config"
	"github.com/jbenet/go-ipfs/core"
	daemon "github.com/jbenet/go-ipfs/daemon2"
	u "github.com/jbenet/go-ipfs/util"
)

// log is the command logger
var log = u.Logger("cmd/ipfs")

// signal to output help
var errHelpRequested = errors.New("Help Requested")

const (
	cpuProfile  = "ipfs.cpuprof"
	heapProfile = "ipfs.memprof"
	errorFormat = "ERROR: %v\n\n"
)

type cmdInvocation struct {
	path []string
	cmd  *cmds.Command
	root *cmds.Command
	req  cmds.Request
}

// main roadmap:
// - parse the commandline to get a cmdInvocation
// - if user requests, help, print it and exit.
// - run the command invocation
// - output the response
// - if anything fails, print error, maybe with help
func main() {
	var invoc cmdInvocation
	var err error

	// we'll call this local helper to output errors.
	// this is so we control how to print errors in one place.
	printErr := func(err error) {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err.Error())
	}

	// this is a local helper to print out help text.
	// there's some considerations that this makes easier.
	printHelp := func(long bool) {
		helpFunc := cmdsCli.ShortHelp
		if long {
			helpFunc = cmdsCli.LongHelp
		}

		helpFunc("ipfs", invoc.root, invoc.path, os.Stderr)
	}

	// parse the commandline into a command invocation
	err = invoc.Parse(os.Args[1:])

	// BEFORE handling the parse error, if we have enough information
	// AND the user requested help, print it out and exit
	if invoc.cmd != nil {
		longH, shortH, err := invoc.requestedHelp()
		if err != nil {
			printErr(err)
			os.Exit(1)
		}
		if longH || shortH {
			printHelp(longH)
			os.Exit(0)
		}
	}

	// ok now handle handle parse error (which means cli input was wrong,
	// e.g. incorrect number of args, or nonexistent subcommand)
	if err != nil {
		printErr(err)

		// this was a user error, print help.
		if invoc.cmd != nil {
			// we need a newline space.
			fmt.Fprintf(os.Stderr, "\n")
			printHelp(false)
		}
		os.Exit(1)
	}

	// ok, finally, run the command invocation.
	output, err := invoc.Run()
	if err != nil {
		printErr(err)

		// if this error was a client error, print short help too.
		if isClientError(err) {
			printHelp(false)
		}
		os.Exit(1)
	}

	// everything went better than expected :)
	io.Copy(os.Stdout, output)
}

func (i *cmdInvocation) Run() (output io.Reader, err error) {
	handleInterrupt()

	// check if user wants to debug. option OR env var.
	debug, _, err := i.req.Option("debug").Bool()
	if err != nil {
		return nil, err
	}
	if debug || u.GetenvBool("DEBUG") {
		u.Debug = true
		u.SetAllLoggers(logging.DEBUG)
	}

	// if debugging, let's profile.
	// TODO maybe change this to its own option... profiling makes it slower.
	if u.Debug {
		stopProfilingFunc, err := startProfiling()
		if err != nil {
			return nil, err
		}
		defer stopProfilingFunc() // to be executed as late as possible
	}

	res, err := callCommand(i.req, i.root)
	if err != nil {
		return nil, err
	}

	return res.Reader()
}

func (i *cmdInvocation) Parse(args []string) error {
	var err error

	i.req, i.root, i.cmd, i.path, err = cmdsCli.Parse(args, Root)
	if err != nil {
		return err
	}

	configPath, err := getConfigRoot(i.req)
	if err != nil {
		return err
	}

	conf, err := getConfig(configPath)
	if err != nil {
		return err
	}
	ctx := i.req.Context()
	ctx.ConfigRoot = configPath
	ctx.Config = conf

	// if no encoding was specified by user, default to plaintext encoding
	// (if command doesn't support plaintext, use JSON instead)
	if !i.req.Option("encoding").Found() {
		if i.req.Command().Marshallers != nil && i.req.Command().Marshallers[cmds.Text] != nil {
			i.req.SetOption("encoding", cmds.Text)
		} else {
			i.req.SetOption("encoding", cmds.JSON)
		}
	}

	return nil
}

func (i *cmdInvocation) requestedHelp() (short bool, long bool, err error) {
	longHelp, _, err := i.req.Option("help").Bool()
	if err != nil {
		return false, false, err
	}
	shortHelp, _, err := i.req.Option("h").Bool()
	if err != nil {
		return false, false, err
	}
	return longHelp, shortHelp, nil
}

func callCommand(req cmds.Request, root *cmds.Command) (cmds.Response, error) {
	var res cmds.Response

	// TODO explain what it means when root == Root
	// @mappum o/
	if root == Root {
		res = root.Call(req)

	} else {
		local, found, err := req.Option("local").Bool()
		if err != nil {
			return nil, err
		}

		remote := !found || !local

		log.Info("Checking if daemon is running...")
		if remote && daemon.Locked(req.Context().ConfigRoot) {
			addr, err := ma.NewMultiaddr(req.Context().Config.Addresses.API)
			if err != nil {
				return nil, err
			}

			_, host, err := manet.DialArgs(addr)
			if err != nil {
				return nil, err
			}

			client := cmdsHttp.NewClient(host)

			res, err = client.Send(req)
			if err != nil {
				return nil, err
			}

		} else {
			log.Info("Executing command locally: daemon not running")
			node, err := core.NewIpfsNode(req.Context().Config, false)
			if err != nil {
				return nil, err
			}
			defer node.Close()
			req.Context().Node = node

			res = root.Call(req)
		}
	}

	return res, nil
}

func isClientError(err error) bool {
	// cast to cmds.Error
	cmdErr, ok := err.(*cmds.Error)
	if !ok {
		return false
	}

	// here we handle the case where commands with
	// no Run func are invoked directly. As help requests.
	if err == cmds.ErrNotCallable {
		return true
	}

	return cmdErr.Code == cmds.ErrClient
}

func getConfigRoot(req cmds.Request) (string, error) {
	configOpt, found, err := req.Option("config").String()
	if err != nil {
		return "", err
	}
	if found && configOpt != "" {
		return configOpt, nil
	}

	configPath, err := config.PathRoot()
	if err != nil {
		return "", err
	}
	return configPath, nil
}

func getConfig(path string) (*config.Config, error) {
	configFile, err := config.Filename(path)
	if err != nil {
		return nil, err
	}

	return config.Load(configFile)
}

// startProfiling begins CPU profiling and returns a `stop` function to be
// executed as late as possible. The stop function captures the memprofile.
func startProfiling() (func(), error) {

	// start CPU profiling as early as possible
	ofi, err := os.Create(cpuProfile)
	if err != nil {
		return nil, err
	}
	pprof.StartCPUProfile(ofi)

	stopProfiling := func() {
		pprof.StopCPUProfile()
		defer ofi.Close() // captured by the closure
		err := writeHeapProfileToFile()
		if err != nil {
			log.Critical(err)
		}
	}
	return stopProfiling, nil
}

func writeHeapProfileToFile() error {
	mprof, err := os.Create(heapProfile)
	if err != nil {
		return err
	}
	defer mprof.Close() // _after_ writing the heap profile
	return pprof.WriteHeapProfile(mprof)
}

// listen for and handle SIGTERM
func handleInterrupt() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)

	go func() {
		for _ = range c {
			log.Info("Received interrupt signal, terminating...")
			os.Exit(0)
		}
	}()
}
