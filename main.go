package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
)

type Action int

const (
	PrintHelpAction Action = iota
	RunSingleAction
	RunAllAction
	RemoveConfigAction
)

type Args struct {
	action   Action
	index    int
	topoName string
}

var helpString = string(`Usage:
  $ sudo ./hstf OP | <test-number>

  where OP is one of:

  all			- run all tests
  help			- print this help
  rm <topo>		- manually remove topology configuration from system
`)

func printHelp() {
	fmt.Printf("%s\n\nDefined tests:\n", helpString)
	PrintTestDefinitions()
}

func parseArgs(args *Args) error {
	argLength := len(os.Args[1:])
	if argLength < 1 {
		printHelp()
		return errors.New("arguments required")
	}

	if os.Args[1] == "all" {
		args.action = RunAllAction
	} else if os.Args[1] == "help" {
		args.action = PrintHelpAction
	} else if os.Args[1] == "rm" {
		args.action = RemoveConfigAction
		args.topoName = os.Args[2]
	} else {
		index, err := strconv.Atoi(os.Args[1])
		if err != nil {
			return errors.New("arg not a number")
		}
		args.action = RunSingleAction
		args.index = index
	}
	return nil
}

func main() {
	var args Args

	err := parseArgs(&args)
	if err != nil {
		fmt.Printf("Parse error: %v\n", err)
		os.Exit(1)
	}

	if args.action == PrintHelpAction {
		printHelp()
		os.Exit(0)
	}

	err = InitFramework()
	if err != nil {
		fmt.Println("error on framework init: %v", err)
		os.Exit(1)
	}
	err = RunTestFw(&args)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}
