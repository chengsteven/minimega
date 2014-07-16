// Copyright (2014) Sandia Corporation.
// Under the terms of Contract DE-AC04-94AL85000 with Sandia Corporation,
// the U.S. Government retains certain rights in this software.

package main

import (
	"errors"
	"io/ioutil"
	log "minilog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	INJECT_COMMAND = iota
	GET_BACKING_IMAGE_COMMAND
)

type injectPair struct {
	src string
	dst string
}

type injectData struct {
	childImg  string
	srcImg    string
	dstImg    string
	partition string
	nPairs    int
	nbdPath   string
	injPairs  []injectPair
}

//Parse the source-file:destination pairs
func (inject *injectData) parseInjectPairs(c cliCommand, argIdx int) error {
	var args string
	parseSrc := true
	injIdx := 0

	//parse inject pairs
	args = strings.Join(c.Args[argIdx:], " ")
	quotedTokens := strings.Split(args, "\"")
	for _, quotedTok := range quotedTokens {
		if quotedTok == "" || quotedTok == " " {
			continue
		}

		//if there is no ":", path is quoted
		if !strings.Contains(quotedTok, ":") {
			if parseSrc {
				inject.injPairs[injIdx].src = quotedTok
				parseSrc = false
			} else {
				inject.injPairs[injIdx].dst = quotedTok
				parseSrc = true
				injIdx++
			}
			continue
		} else {
			//nothing in this token was quoted,
			//so both spaces and : split arguments
			st := strings.Replace(quotedTok, ":", " ", -1)
			toks := strings.Split(st, " ")
			for _, tok := range toks {
				if tok == "" || tok == " " {
					continue
				}

				if parseSrc {
					inject.injPairs[injIdx].src = tok
					parseSrc = false
				} else {
					inject.injPairs[injIdx].dst = tok
					parseSrc = true
					injIdx++
				}
			}
		}
	}

	inject.nPairs = injIdx

	if !parseSrc {
		return errors.New("malformed command")
	}

	return nil
}

//Parse the command line to get the arguments for vm_inject
func (inject *injectData) parseInject(c cliCommand) error {
	argIdx := 1
	var dstImgStr string
	var dstImg *os.File
	var err error

	switch {
	case len(c.Args) == 0:
		return errors.New("malformed command")
	case len(c.Args) == 1:
		inject.childImg = c.Args[0]
		return nil
	case len(c.Args) > 1:
		inject.injPairs = make([]injectPair, len(c.Args)-1)

		//parse source image
		srcPair := strings.Split(c.Args[0], ":")
		inject.srcImg, err = filepath.Abs(srcPair[0])
		if err != nil {
			return err
		}
		if len(srcPair) == 2 {
			inject.partition = srcPair[1]
		}

		//parse destination image
		if !strings.Contains(c.Args[1], ":") {
			if strings.Contains(c.Args[1], "/") {
				return errors.New("dst image path must not be absolute")
			}
			dstImgStr = *f_iomBase + c.Args[1]
			argIdx++
		} else {
			dstImg, err = ioutil.TempFile(*f_iomBase, "snapshot")
			dstImgStr = dstImg.Name()
			if err != nil {
				return errors.New("could not create a dst image")
			}
		}
		inject.dstImg = dstImgStr

		return inject.parseInjectPairs(c, argIdx)

	}
	return nil
}

//Unmount, disconnect nbd, and remove mount directory
func vmInjectCleanup(mntDir, nbdPath string) {
	p := process("umount")
	cmd := exec.Command(p, mntDir)
	err := cmd.Run()
	if err != nil {
		log.Error("vmInjectCleanup: %v", err)
	}

	p = process("qemu-nbd")
	cmd = exec.Command(p, "-d", nbdPath)
	err = cmd.Run()
	if err != nil {
		log.Error("qemu nbd disconnect: %v", err)
		log.Warn("minimega was unable to disconnect %v", nbdPath)
	}

	p = process("rm")
	cmd = exec.Command(p, "-r", mntDir)
	err = cmd.Run()
	if err != nil {
		log.Error("rm mount dir: %v", err)
	}
}

//Inject files into a qcow
//Alternatively, this function can also return a qcow2 backing file's name
func cliVMInject(c cliCommand) cliResponse {
	r := cliResponse{}
	inject := injectData{}

	// yell at user to load nbd
	p := process("lsmod")
	cmd := exec.Command(p)
	result, err := cmd.CombinedOutput()
	if err != nil {
		r.Error = err.Error()
		return r
	}

	if !strings.Contains(string(result), "nbd ") {
		r.Error = "insert module 'nbd'"
		return r
	}

	// yell at user to load nbd with a max_part parameter
	_, err = os.Stat("/sys/module/nbd/parameters/max_part")
	if err != nil {
		log.Warn("no max_part parameter set for module nbd\n" +
			"set it if using images with multiple partitions")
	}

	err = inject.parseInject(c)
	if err != nil {
		r.Error = err.Error()
		return r
	}

	if inject.childImg != "" {
		p := process("qemu-img")
		cmd := exec.Command(p, "info", inject.childImg)
		parent, err := cmd.Output()
		if err != nil {
			r.Error = err.Error()
		} else {
			r.Response = string(parent[:])
		}
		return r
	}

	//create the new img
	p = process("qemu-img")
	cmd = exec.Command(p, "create", "-f", "qcow2", "-b", inject.srcImg, inject.dstImg)
	result, err = cmd.CombinedOutput()
	if err != nil {
		r.Error = string(result[:]) + "\n" + err.Error()
		return r
	}

	// create a tmp mount point
	mntDir, err := ioutil.TempDir(*f_base, "dstImg")
	if err != nil {
		r.Error = err.Error()
		return r
	}

	// check for nbds
	devFiles, err := ioutil.ReadDir("/dev")
	if err != nil {
		r.Error = err.Error()
		return r
	}

	nbds := []string{}
	for _, file := range devFiles {
		// we don't want to include partitions here
		if strings.Contains(file.Name(), "nbd") && !strings.Contains(file.Name(), "p") {
			nbds = append(nbds, file.Name())
		}
	}

	// select first available nbd
	for _, nbd := range nbds {

		// check whether a pid exists for the current nbd
		_, err = os.Stat("/sys/block/" + nbd + "/pid")
		if err != nil {
			log.Debug("trying: " + nbd)
			inject.nbdPath = "/dev/" + nbd
			break
		} else {
			log.Debug("nbd %v could not be used", nbd)
		}
	}

	if inject.nbdPath == "" {
		r.Error = "no available nbds found"
		return r
	}

	//connect new img to nbd
	p = process("qemu-nbd")
	cmd = exec.Command(p, "-c", inject.nbdPath, inject.dstImg)
	result, err = cmd.CombinedOutput()
	if err != nil {
		vmInjectCleanup(mntDir, inject.nbdPath)
		r.Error = string(result[:]) + "\n" + err.Error()
		return r
	}

	time.Sleep(100 * time.Millisecond) //give time to create partitions

	//decide on a partition
	if inject.partition == "" {
		_, err = os.Stat(inject.nbdPath + "p1")
		if err != nil {
			vmInjectCleanup(mntDir, inject.nbdPath)
			r.Error = "no partitions found"
			return r
		}

		_, err = os.Stat(inject.nbdPath + "p2")
		if err == nil {
			vmInjectCleanup(mntDir, inject.nbdPath)
			r.Error = "please specify a partition; multiple found"
			return r
		}

		inject.partition = "1"
	}

	//mount new img
	p = process("mount")
	cmd = exec.Command(p, "-w", inject.nbdPath+"p"+inject.partition,
		mntDir)
	result, err = cmd.CombinedOutput()
	if err != nil {
		//if mount failed, try ntfs-3g

		// check for ntfs support in the kernel
		_, err = os.Stat("/lib/modules/`uname -r`/kernel/fs/ntfs")
		if err != nil {
			r.Error = "no ntfs suport found in current kernel"
			return r
		}

		// check that ntfs-3g is installed
		p = process("which")
		cmd = exec.Command(p, "ntfs-3g")
		_, err = cmd.CombinedOutput()
		if err != nil {
			log.Warn("ntfs-3g not found, image may not be writeable")
		}

		// mount with ntfs-3g
		p = process("mount")
		cmd = exec.Command(p, "-o", "ntfs-3g", inject.nbdPath+"p"+inject.partition, mntDir)
		result, err = cmd.CombinedOutput()
		if err != nil {
			log.Error("failed to mount")
			vmInjectCleanup(mntDir, inject.nbdPath)
			r.Error = string(result[:]) + "\n" + err.Error()
			return r
		}
	}

	//copy files/folders in
	for i := 0; i < inject.nPairs; i++ {
		p = process("cp")
		dir := filepath.Dir(mntDir + "/" + inject.injPairs[i].dst)
		os.MkdirAll(dir, 0775)
		cmd = exec.Command(p, "-fr", inject.injPairs[i].src, mntDir+"/"+inject.injPairs[i].dst)
		result, err = cmd.CombinedOutput()
		if err != nil {
			vmInjectCleanup(mntDir, inject.nbdPath)
			r.Error = string(result[:]) + "\n" + err.Error()
			return r
		}
	}

	log.Debug("Cleaning up vm_inject")
	vmInjectCleanup(mntDir, inject.nbdPath)

	r.Response = inject.dstImg

	return r
}
