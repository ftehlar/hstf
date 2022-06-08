package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"os/signal"
	"sync"

	"git.fd.io/govpp.git/api"
	"github.com/edwarnicke/vpphelper"
	"github.com/networkservicemesh/sdk/pkg/tools/log"
	"github.com/networkservicemesh/sdk/pkg/tools/log/logruslogger"
)

type ConfFn func(context.Context, api.Connection) error
type TestFn func() error

var testMatrix = []TestDesc{
	{TestLDPreloadIperfVpp, "2peerVeth", "LD preload iperf (VPP)"},
	{TestIperfLinux, "tap", "iperf3 (Linux)"},
	{TestHttpTps, "", "HTTP tps test"},
	{TestVppProxyHttpTcp, "ns", "HTTP/TCP  Vpp Proxy"},
	{TestEnvoyProxyHttpTcp, "ns", "Envoy HTTP/TCP Proxy"},
}

type TestDesc struct {
	fn       TestFn
	topoName string
	desc     string
}

type TestCase struct {
	fn       TestFn
	topo     *Topo
	desc     string
	topoName string
	result   error
}

type TcContext struct {
	wg     *sync.WaitGroup
	mainCh chan context.CancelFunc
}

type Config struct {
	LdPreload string `json:"ld_preload"`
}

var colorReset = "\033[0m"
var colorRed = "\033[31m"
var colorGreen = "\033[32m"
var colorPurple = "\033[35m"

var topoBase TopoBase
var tests []TestCase
var config Config

func (c *Config) Load(confName string) error {
	file, err := ioutil.ReadFile(confName)
	if err != nil {
		return err
	}
	return json.Unmarshal(file, c)
}

func (tc *TcContext) init(nInst int) {
	var wg sync.WaitGroup
	wg.Add(nInst)
	tc.wg = &wg
	tc.mainCh = make(chan context.CancelFunc)
}

func startVpp(tc *TcContext, instance string, startupCofnig *Stanza, confFn ConfFn) {
	ctx, cancel := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
	)

	tc.mainCh <- cancel

	path := fmt.Sprintf("/tmp/%s", instance)
	log.EnableTracing(true)
	ctx = log.WithLog(ctx, logruslogger.New(ctx, map[string]interface{}{"cmd": os.Args[0]}))

	con, vppErrCh := vpphelper.StartAndDialContext(ctx, vpphelper.WithRootDir(path),
		vpphelper.WithStanza(startupCofnig.ToString()))
	exitOnErrCh(ctx, cancel, vppErrCh)

	err := confFn(ctx, con)
	if err != nil {
		log.FromContext(ctx).Errorf("configuration failed: %s", err)
	}
	// notify main thread that configuration is finished
	tc.wg.Done()
	log.FromContext(ctx).Infof("cli socket: %s/var/run/vpp/cli.sock", path)
	<-ctx.Done()
}

func Vppcli(inst, command string) {
	cmd := exec.Command("vppctl", "-s", fmt.Sprintf("/tmp/%s/var/run/vpp/cli.sock", inst), command)
	o, err := cmd.CombinedOutput()
	if err != nil {
		log.Default().Errorf("failed to execute command: '%s'.\n", err)
	}
	log.Default().Debugf("Command output %s", string(o))
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

func getResultString(tc *TestCase) (bool, string) {
	if tc.result == nil {
		return true, string(colorGreen) + "Passed" + string(colorReset)
	}
	return false, string(colorRed) + "Failed" + string(colorReset)
}

func printResults(tests []TestCase) {
	color := colorGreen
	fmt.Println("\nResults:")
	for i, tc := range tests {
		res, str := getResultString(&tc)
		if !res {
			color = colorRed
		}
		fmt.Printf("%d. %s%s%s\t%s\n", i, string(color), str, string(colorReset), tc.desc)
	}
}

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

func runAllTests(tests []TestCase) error {
	for _, tc := range tests {
		runSingleTest(&tc)
	}
	printResults(tests)
	return nil
}

func registerTestCase(fn TestFn, desc string, topo *Topo, topoName string) {
	tests = append(tests, TestCase{fn, topo, desc, topoName, nil})
}

func PrintTestDefinitions() {
	for i, t := range testMatrix {
		fmt.Printf(" %d %s [%s]\n", i, t.desc, t.topoName)
	}
}

func PrintTests() {
	for i, t := range tests {
		fmt.Printf(" %d %s (%p)\n", i, t.desc, t.topo)
	}
}

func InitFramework() error {
	const topoDir = "topo/"

	err := topoBase.LoadTopologies(topoDir)
	if err != nil {
		return fmt.Errorf("error on loading topology definitions: %v", err)
	}

	err = config.Load("config.json")
	if err != nil {
		return fmt.Errorf("error on loading config file: %v", err)
	}

	for _, t := range testMatrix {
		if t.topoName != "" {
			topo := topoBase.FindTopoByName(t.topoName)
			if topo == nil {
				return errors.New("topo not found")
			}
			registerTestCase(t.fn, t.desc, topo, t.topoName)
		} else {
			registerTestCase(t.fn, t.desc, nil, t.topoName)
		}
	}
	return nil
}

func runSingleTest(t *TestCase) error {
	t.result = fmt.Errorf("unspecified error")
	if t.topo != nil {
		fmt.Printf("Configuring topology %s for %s\n", t.topoName, t.desc)
		err := t.topo.Configure()
		if err != nil {
			return fmt.Errorf("failed to prepare topology: %v", err)
		}
		defer t.topo.RemoveConfig()
	} else {
		fmt.Println("No topology defined for", t.desc)
	}

	fmt.Println(string(colorPurple) + "Starting test case " + t.desc + string(colorReset))
	t.result = t.fn()
	fmt.Println(string(colorPurple) + "End of test case: " + t.desc + string(colorReset))
	_, s := getResultString(t)
	fmt.Println(s)
	return t.result
}

func RunTestFw(a *Args) error {
	switch a.action {
	case RunSingleAction:
		if a.index >= len(tests) {
			return fmt.Errorf("invalid test index")
		}
		return runSingleTest(&tests[a.index])
	case RunAllAction:
		return runAllTests(tests)
	case RemoveConfigAction:
		topo := topoBase.FindTopoByName(a.topoName)
		if topo == nil {
			return fmt.Errorf("topology %s not found", a.topoName)
		}
		topo.RemoveConfig()
		return nil
	}
	return fmt.Errorf("no option specified")
}
