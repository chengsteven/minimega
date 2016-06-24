package main

import (
	"bufio"
	"minicli"
	log "minilog"
	"net"
	"path/filepath"
)

func commandSocketStart() {
	l, err := net.Listen("unix", filepath.Join(*f_path, "minirouter"))
	if err != nil {
		log.Fatalln("commandSocketStart: %v", err)
	}

	for {
		conn, err := l.Accept()
		if err != nil {
			log.Error("commandSocketStart: accept: %v", err)
		}
		log.Infoln("client connected")

		go commandSocketHandle(conn)
	}
}

func commandSocketHandle(conn net.Conn) {
	// just read comments off the wire
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		line := scanner.Text()
		log.Debug("got command: %v", line)
		r, err := minicli.ProcessString(line, false)
		if err != nil {
			log.Errorln(err)
		}
		<-r
	}
	if err := scanner.Err(); err != nil {
		log.Errorln(err)
	}
	conn.Close()
}
