package hstf

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sync"

	"git.fd.io/govpp.git/api"
	"github.com/edwarnicke/vpphelper"
	"github.com/networkservicemesh/sdk/pkg/tools/log"
	"github.com/networkservicemesh/sdk/pkg/tools/log/logruslogger"
)

const configTemplate = `unix {
  nodaemon
  log %[1]s/var/log/vpp/vpp.log
  full-coredump
  cli-listen %[1]s/var/run/vpp/cli.sock
  runtime-dir %[1]s/var/run
  gid vpp
}

api-trace {
  on
}

api-segment {
  gid vpp
}

socksvr {
  socket-name %[1]s/var/run/vpp/api.sock
}

statseg {
  socket-name %[1]s/var/run/vpp/stats.sock
}

%[2]s
`

type ConfFn func(context.Context, api.Connection) error

type TcContext struct {
	wg *sync.WaitGroup
}

func (tc *TcContext) init(nInst int) {
	var wg sync.WaitGroup
	wg.Add(nInst)
	tc.wg = &wg
}

func newVppContext() (context.Context, context.CancelFunc) {
	ctx, cancel := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
	)
	ctx = log.WithLog(ctx, logruslogger.New(ctx, map[string]interface{}{"cmd": os.Args[0]}))
	return ctx, cancel
}

func startVpp(ctx context.Context, tc *TcContext, cancel context.CancelFunc, runDir, startupConfig string,
	confFn ConfFn) {
	con, vppErrCh := vpphelper.StartAndDialContext(ctx, vpphelper.WithVppConfig(startupConfig),
		vpphelper.WithRootDir(runDir))
	exitOnErrCh(ctx, cancel, vppErrCh)

	err := confFn(ctx, con)
	if err != nil {
		log.FromContext(ctx).Errorf("configuration failed: %s", err)
	}
	// notify main thread that configuration is finished
	tc.wg.Done()
	log.FromContext(ctx).Infof("cli socket: %s/var/run/vpp/cli.sock", runDir)
	<-ctx.Done()
}

func Vppcli(runDir, command string) {
	cmd := exec.Command("vppctl", "-s", fmt.Sprintf("%s/var/run/vpp/cli.sock", runDir), command)
	o, err := cmd.CombinedOutput()
	if err != nil {
		log.Default().Errorf("failed to execute command: '%s'.\n", err)
	}
	log.Default().Debugf("Command output %s", string(o))
}

func startHttpServer(running chan struct{}, done chan struct{}, addressPort, netNs string) {
	cmd := NewCommand([]string{"./tools/http_server/http_server", addressPort}, netNs)
	err := cmd.Start()
	if err != nil {
		fmt.Println("Failed to start http server")
		return
	}
	running <- struct{}{}
	<-done
	cmd.Process.Kill()
}

func startWget(finished chan error, server_ip, port string) {
	fname := "test_file_10M"
	defer func() {
		finished <- errors.New("wget error")
	}()

	cmd := exec.Command("wget", "-q", "-O", "/dev/null", server_ip+":"+port+"/"+fname)
	o, err := cmd.CombinedOutput()
	if err != nil {
		log.Default().Errorf("wget error: '%s'.\n%s", err, o)
		return
	}
	log.Default().Debugf("Client output: %s", o)
	finished <- nil
}

/* func createtempDir() string {
	dir, err := ioutil.TempDir("/tmp", "hstf-vpp-*")
	if err != nil {
		fmt.Println("error creating temp dir: ", err)
		return ""
	}
	return dir
} */

/* func checkAppExists(name string) {
    path, err := exec.LookPath(name)
    if err != nil {
        fmt.Printf("didn't find '%s' executable\n", name)
    }
} */

func exitOnErrCh(ctx context.Context, cancel context.CancelFunc, errCh <-chan error) {
	// If we already have an error, log it and exit
	select {
	case err := <-errCh:
		log.FromContext(ctx).Fatal(err)
	default:
	}
	go func(ctx context.Context, errCh <-chan error) {
		<-errCh
		cancel()
	}(ctx, errCh)
}
