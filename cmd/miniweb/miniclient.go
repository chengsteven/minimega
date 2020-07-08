// Copyright (2017) Sandia Corporation.
// Under the terms of Contract DE-AC04-94AL85000 with Sandia Corporation,
// the U.S. Government retains certain rights in this software.

package main

import (
	"github.com/sandia-minimega/minimega/internal/miniclient"
	log "github.com/sandia-minimega/minimega/pkg/minilog"
	"strings"
	"sync"
)

var mmMu sync.Mutex
var mm *miniclient.Conn

// noOp returns a closed channel
func noOp() chan *miniclient.Response {
	out := make(chan *miniclient.Response)
	close(out)
	return out
}

// run minimega commands, automatically redialing if we were disconnected
func run(c *Command) chan *miniclient.Response {
	mmMu.Lock()
	defer mmMu.Unlock()

	var err error

	if mm == nil {
		if mm, err = miniclient.Dial(*f_base); err != nil {
			log.Error("unable to dial: %v", err)
			return noOp()
		}
	}

	// check if there's already an error and try to redial
	if err := mm.Error(); err != nil {
		s := err.Error()
		if strings.Contains(s, "broken pipe") || strings.Contains(s, "no such file or directory") {
			if mm, err = miniclient.Dial(*f_base); err != nil {
				log.Error("unable to redial: %v", err)
				return noOp()
			}
		} else {
			return noOp()
		}
	}

	log.Info("running: %v", c)
	return mm.Run(c.String())
}
