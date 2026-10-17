// SPDX-License-Identifier: Apache-2.0
// Copyright Tony Burns

// Command nastools is the nastools command-line tool.
//
// See Run in run.go.
package main

import "os"

func main() { os.Exit(Run(os.Args[1:], streams{in: os.Stdin, out: os.Stdout, err: os.Stderr})) }
