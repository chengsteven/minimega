// Copyright (2012) Sandia Corporation.
// Under the terms of Contract DE-AC04-94AL85000 with Sandia Corporation,
// the U.S. Government retains certain rights in this software.
//
// David Fritz <djfritz@sandia.gov>

// command line interface for minimega
//
// The command line interface wraps a number of commands listed in the
// cliCommands map. Each entry to the map defines a function that is called
// when the command is invoked on the command line, as well as short and long
// form help. The record parameter instructs the cli to put the command in the
// command history.
//
// The cli uses the readline library for command history and tab completion.
// A separate command history is kept and used for writing the buffer out to
// disk.

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"gomacro"
	"goreadline"
	"io"
	log "minilog"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
	"version"
)

const (
	COMMAND_TIMEOUT = 10
)

var (
	commandBuf []string // command history for the write command

	// incoming commands for the cli to parse. these can come from the cli
	// proper (readline), or from a network source, etc. the cli will parse
	// them all as if they were typed locally.
	commandChanLocal   chan cliCommand
	commandChanSocket  chan cliCommand
	commandChanMeshage chan cliCommand

	ackChanLocal   chan cliResponse // acknowledgements from the cli, one per incoming command
	ackChanSocket  chan cliResponse
	ackChanMeshage chan cliResponse

	cliCommands map[string]*command

	macro *gomacro.Macro
)

type cliCommand struct {
	Command string
	Args    []string
	ackChan chan cliResponse
	TID     int32
}

type cliResponse struct {
	Response string
	Error    string // because you can't gob/json encode an error type
	More     bool   // more is set if the called command will be sending multiple responses
	TID      int32
}

type command struct {
	Call      func(c cliCommand) cliResponse // callback function
	Helpshort string                         // short form help test, one line only
	Helplong  string                         // long form help text
	Record    bool                           // record in the command history
	Clear     func() error                   // clear/restore to default state
}

func init() {
	commandChanLocal = make(chan cliCommand)
	commandChanSocket = make(chan cliCommand)
	commandChanMeshage = make(chan cliCommand)
	ackChanLocal = make(chan cliResponse)
	ackChanSocket = make(chan cliResponse)
	ackChanMeshage = make(chan cliResponse)

	macro = gomacro.NewMacro()

	// list of commands the cli supports. some commands have small callbacks, which
	// are defined inline.
	cliCommands = map[string]*command{
		"log_level": &command{
			Call:      cliLogLevel,
			Helpshort: "set the log level",
			Helplong: `
	Usage: log_level [debug, info, warn, error, fatal]

Set the log level to one of [debug, info, warn, error, fatal]. Log levels
inherit lower levels, so setting the level to error will also log fatal, and
setting the mode to debug will log everything.`,
			Record: true,
			Clear: func() error {
				*f_loglevel = "error"
				log.SetLevel("stdio", log.ERROR)
				log.SetLevel("file", log.ERROR)
				return nil
			},
		},

		"log_stderr": &command{
			Call:      cliLogStderr,
			Helpshort: "enable/disable logging to stderr",
			Helplong: `
	Usage: log_stderr [true, false]

Enable or disable logging to stderr. Valid options are [true, false].`,
			Record: true,
			Clear: func() error {
				_, err := log.GetLevel("stdio")
				if err == nil {
					log.DelLogger("stdio")
				}
				return nil
			},
		},

		"log_file": &command{
			Call:      cliLogFile,
			Helpshort: "enable logging to a file",
			Helplong: `
	Usage: log_file [filename]

Log to a file. To disable file logging, call "clear log_file".`,
			Record: true,
			Clear: func() error {
				_, err := log.GetLevel("file")
				if err == nil {
					log.DelLogger("file")
				}
				return nil
			},
		},

		"check": &command{
			Call:      externalCheck,
			Helpshort: "check for the presence of all external executables minimega uses",
			Helplong: `
	Usage: check

Minimega maintains a list of external packages that it depends on, such as qemu.
Calling check will attempt to find each of these executables in the avaiable
path, and returns an error on the first one not found.`,
			Record: true,
			Clear: func() error {
				return nil
			},
		},

		"nuke": &command{
			Call:      nuke,
			Helpshort: "attempt to clean up after a crash",
			Helplong: `
	Usage: nuke

After a crash, the VM state on the machine can be difficult to recover from.
Nuke attempts to kill all instances of QEMU, remove all taps and bridges, and
removes the temporary minimega state on the harddisk.`,
			Record: true,
			Clear: func() error {
				return nil
			},
		},

		"write": &command{
			Call: func(c cliCommand) cliResponse {
				if len(c.Args) != 1 {
					return cliResponse{
						Error: "write takes a single argument",
					}
				}
				file, err := os.Create(c.Args[0])
				if err != nil {
					return cliResponse{
						Error: err.Error(),
					}
				}
				for _, i := range commandBuf {
					_, err = file.WriteString(i + "\n")
					if err != nil {
						return cliResponse{
							Error: err.Error(),
						}
					}
				}
				return cliResponse{}
			},
			Helpshort: "write the command history to a file",
			Helplong: `
	Usage: write <file>

Write the command history to file. This is useful for handcrafting configs
on the minimega command line and then saving them for later use. Args that
failed, as well as some commands that do not impact the VM state, such as
'help', do not get recorded.`,
			Record: false,
			Clear: func() error {
				return nil
			},
		},

		"vm_save": &command{
			Call:      cliVMSave,
			Helpshort: "save a vm configuration for later use",
			Helplong: `
	Usage: vm_save <save name> <vm name/id> [<vm name/id> ...]

Saves the configuration of a running virtual machine or set of virtual 
machines so that it/they can be restarted/recovered later, such as after 
a system crash.

If no VM name or ID is given, all VMs (including those in the quit and error state) will be saved.

This command does not store the state of the virtual machine itself, 
only its launch configuration.`,
			Record: false,
			Clear: func() error {
				return nil
			},
		},

		"read": &command{
			Call: func(c cliCommand) cliResponse {
				if len(c.Args) != 1 {
					return cliResponse{
						Error: "read takes a single argument",
					}
				}
				file, err := os.Open(c.Args[0])
				if err != nil {
					return cliResponse{
						Error: err.Error(),
					}
				}
				r := bufio.NewReader(file)
				for {
					l, _, err := r.ReadLine()
					if err != nil {
						if err == io.EOF {
							break
						} else {
							return cliResponse{
								Error: err.Error(),
							}
						}
					}
					log.Debug("read command: %v", string(l)) // commands don't have their newlines removed
					resp := cliExec(makeCommand(string(l)))
					resp.More = true
					c.ackChan <- resp
					if resp.Error != "" {
						break // stop on errors
					}
				}
				return cliResponse{}
			},
			Helpshort: "read and execute a command file",
			Helplong: `
	Usage: read <file>

Read a command file and execute it. This has the same behavior as if you typed
the file in manually.`,
			Record: true,
			Clear: func() error {
				return nil
			},
		},

		"vm_info": &command{
			Call: func(c cliCommand) cliResponse {
				return vms.info(c)
			},
			Helpshort: "print information about VMs",
			Helplong: `
	Usage: vm_info [output=<quiet,json>] [search=<search term>] [ [output mask[,...]] ]

Print information about VMs. vm_info allows searching for VMs based on any VM
parameter, and output some or all information about the VMs in question.
Additionally, you can display information about all running VMs. 

A vm_info command takes three optional arguments, an output mode, a search
term, and an output mask. If the search term is omitted, information about all
VMs will be displayed. If the output mask is omitted, all information about the
VMs will be displayed.

The output mode has two options - quiet and json. Two use either, set the output using the following syntax:

	vm_info output=quiet ...

If the output mode is set to 'quiet', the header and "|" characters in the output formatting will be removed. The output will consist simply of tab delimited lines of VM info based on the search and mask terms.

If the output mode is set to 'json', the output will be a json formatted string containing info on all VMs, or those matched by the search term. The mask will be ignored - all fields will be populated.

The search term uses a single key=value argument. For example, if you want all
information about VM 50: 

	vm_info id=50

The output mask uses an ordered list of fields inside [] brackets. For example,
if you want the ID and IPs for all VMs on vlan 100: 

	vm_info vlan=100 [id,ip]

Searchable and maskable fields are:

- host	  : the host that the VM is running on
- id	  : the VM ID, as an integer
- name	  : the VM name, if it exists
- memory  : allocated memory, in megabytes
- vcpus   : the number of allocated CPUs
- disk    : disk image
- initrd  : initrd image
- kernel  : kernel image
- cdrom   : cdrom image
- state   : one of (building, running, paused, quit, error)
- tap	  : tap name
- mac	  : mac address
- ip	  : IPv4 address
- ip6	  : IPv6 address
- vlan	  : vlan, as an integer
- bridge  : bridge name
- append  : kernel command line string

Examples:

Display a list of all IPs for all VMs:
	vm_info [ip,ip6]

Display all information about VMs with the disk image foo.qc2:
	vm_info disk=foo.qc2

Display all information about all VMs:
	vm_info`,
			Record: true,
			Clear: func() error {
				return nil
			},
		},

		"quit": &command{
			Call:      cliQuit,
			Helpshort: "quit",
			Helplong: `
	Usage: quit [delay]

Quit. An optional integer argument X allows deferring the quit call for X
seconds. This is useful for telling a mesh of minimega nodes to quit.

quit will not return a response to the cli, control socket, or meshage, it will
simply exit. meshage connected nodes catch this and will remove the quit node
from the mesh. External tools interfacing minimega must check for EOF on stdout
or the control socket as an indication that minimega has quit.`,
			Record: true, // but how!?
			Clear: func() error {
				return nil
			},
		},

		"exit": &command{ // just an alias to quit
			Call:      cliQuit,
			Helpshort: "exit",
			Helplong: `
	Usage: exit [delay]

An alias to 'quit'.`,
			Record: true, // but how!?
			Clear: func() error {
				return nil
			},
		},

		"vm_launch": &command{
			Call: func(c cliCommand) cliResponse {
				return vms.launch(c)
			},
			Helpshort: "launch virtual machines in a paused state",
			Helplong: `
	Usage: vm_launch <number of vms or vm name>

Launch virtual machines in a paused state, using the parameters
defined leading up to the launch command. Any changes to the VM parameters 
after launching will have no effect on launched VMs.

If you supply a name instead of a number of VMs, one VM with that name will be launched.`,
			Record: true,
			Clear: func() error {
				return nil
			},
		},

		"vm_kill": &command{
			Call: func(c cliCommand) cliResponse {
				return vms.kill(c)
			},
			Helpshort: "kill running virtual machines",
			Helplong: `
	Usage: vm_kill <vm id or name>

Kill a virtual machine by ID or name. Pass -1 to kill all virtual machines.`,
			Record: true,
			Clear: func() error {
				return nil
			},
		},

		"vm_start": &command{
			Call: func(c cliCommand) cliResponse {
				return vms.start(c)
			},
			Helpshort: "start paused virtual machines",
			Helplong: `
	Usage: vm_start [VM ID, name]

Start all or one paused virtual machine. To start all paused virtual machines,
call start without the optional VM ID or name.

Calling vm_start on a quit VM will restart the VM.`,
			Record: true,
			Clear: func() error {
				return nil
			},
		},

		"vm_stop": &command{
			Call: func(c cliCommand) cliResponse {
				return vms.stop(c)
			},
			Helpshort: "stop/pause virtual machines",
			Helplong: `
	Usage: vm_stop [VM ID, name]

Stop all or one running virtual machine. To stop all running virtual machines,
call stop without the optional VM ID or name.

Calling stop will put VMs in a paused state. Start stopped VMs with vm_start.`,
			Record: true,
			Clear: func() error {
				return nil

			},
		},

		"vm_qemu": &command{
			Call:      cliVMQemu,
			Helpshort: "set the qemu process to invoke",
			Helplong: `
	Usage: vm_qemu <path to qemu>

Set the qemu process to invoke. Relative paths are ok.`,
			Record: true,
			Clear: func() error {
				externalProcesses["qemu"] = "qemu-system-x86_64"
				return nil
			},
		},

		"vm_memory": &command{
			Call:      cliVMMemory,
			Helpshort: "set the amount of physical memory for a VM",
			Helplong: `
	Usage: vm_memory [memory in megabytes]

Set the amount of physical memory to allocate in megabytes.`,
			Record: true,
			Clear: func() error {
				info.Memory = VM_MEMORY_DEFAULT
				return nil
			},
		},

		"vm_vcpus": &command{
			Call:      cliVMVCPUs,
			Helpshort: "set the number of virtual CPUs for a VM",
			Helplong: `
	Usage: vm_vcpus [number of CPUs]

Set the number of virtual CPUs to allocate a VM.`,
			Record: true,
			Clear: func() error {
				info.Vcpus = "1"
				return nil
			},
		},

		"vm_disk": &command{
			Call:      cliVMDisk,
			Helpshort: "set a disk image to attach to a VM",
			Helplong: `
	Usage: vm_disk [path to disk image]

Attach a disk to a VM. Any disk image supported by QEMU is a valid parameter.
Disk images launched in snapshot mode may safely be used for multiple VMs.`,
			Record: true,
			Clear: func() error {
				info.DiskPath = ""
				return nil
			},
		},

		"vm_cdrom": &command{
			Call:      cliVMCdrom,
			Helpshort: "set a cdrom image to attach to a VM",
			Helplong: `
	Usage: vm_cdrom [path to cdrom image]

Attach a cdrom to a VM. When using a cdrom, it will automatically be set
to be the boot device.`,
			Record: true,
			Clear: func() error {
				info.CdromPath = ""
				return nil
			},
		},

		"vm_kernel": &command{
			Call:      cliVMKernel,
			Helpshort: "set a kernel image to attach to a VM",
			Helplong: `
	Usage: vm_kernel [path to kernel image]

Attach a kernel image to a VM. If set, QEMU will boot from this image instead
of any disk image.`,
			Record: true,
			Clear: func() error {
				info.KernelPath = ""
				return nil
			},
		},

		"vm_initrd": &command{
			Call:      cliVMInitrd,
			Helpshort: "set a initrd image to attach to a VM",
			Helplong: `
	Usage: vm_initrd [path to initrd]

Attach an initrd image to a VM. Passed along with the kernel image at boot time.`,
			Record: true,
			Clear: func() error {
				info.InitrdPath = ""
				return nil
			},
		},

		"vm_qemu_append": &command{
			Call:      cliVMQemuAppend,
			Helpshort: "add additional arguments for the QEMU command",
			Helplong: `
	Usage: vm_qemu_append [argument [...]]

Add additional arguments to be passed to the QEMU instance. For example:
	vm_qemu_append -serial tcp:localhost:4001`,
			Record: true,
			Clear: func() error {
				info.QemuAppend = nil
				return nil
			},
		},

		"vm_append": &command{
			Call:      cliVMAppend,
			Helpshort: "set an append string to pass to a kernel set with vm_kernel",
			Helplong: `
	Usage: vm_append [append string]

Add an append string to a kernel set with vm_kernel. Setting vm_append without
using vm_kernel will result in an error.

For example, to set a static IP for a linux VM:
	vm_append ip=10.0.0.5 gateway=10.0.0.1 netmask=255.255.255.0 dns=10.10.10.10`,
			Record: true,
			Clear: func() error {
				info.Append = ""
				return nil
			},
		},

		"vm_net": &command{
			Call:      cliVMNet,
			Helpshort: "specify the networks the VM is a member of",
			Helplong: `
	Usage: vm_net [bridge,]<id>[,<mac address>][,<driver>] [[bridge,]<id>[,<mac address>][,<driver>] ...]

Specify the network(s) that the VM is a member of by VLAN. A corresponding VLAN
will be created for each network. Optionally, you may specify the bridge the
interface will be connected on. If the bridge name is omitted, minimega will
use the default 'mega_bridge'. You can also optionally specify the mac
address of the interface to connect to that network. If not specifed, the mac
address will be randomly generated. Additionally, you can optionally specify a
driver for qemu to use. By default, e1000 is used. 

Examples:

To connect a VM to VLANs 1 and 5:
	vm_net 1 5
To connect a VM to VLANs 100, 101, and 102 with specific mac addresses:
	vm_net 100,00:00:00:00:00:00 101,00:00:00:00:01:00 102,00:00:00:00:02:00
To connect a VM to VLAN 1 on bridge0 and VLAN 2 on bridge1:
	vm_net bridge0,1 bridge1,2
To connect a VM to VLAN 100 on bridge0 with a specific mac:
	vm_net bridge0,100,00:11:22:33:44:55
To specify a specific driver, such as i82559c:
	vm_net 100,i82559c

Calling vm_net with no parameters will list the current networks for this VM.`,
			Record: true,
			Clear: func() error {
				info.Networks = []int{}
				return nil
			},
		},

		"web": &command{
			Call:      WebCLI,
			Helpshort: "start the minimega web interface",
			Helplong: `
	Usage: web [port, novnc <novnc path>]

Launch a webserver that allows you to browse the connected minimega hosts and 
VMs, and connect to any VM in the pool.

This command requires access to an installation of novnc. By default minimega
looks in 'pwd'/misc/novnc. To set a different path, invoke:

	web novnc <path to novnc>

To start the webserver on a specific port, issue the web command with the port:

	web 7000

8080 is the default port.`,
			Record: true,
			Clear: func() error {
				vncNovnc = "misc/novnc"
				return nil
			},
		},

		"history": &command{
			Call: func(c cliCommand) cliResponse {
				r := cliResponse{}
				if len(c.Args) != 0 {
					r.Error = "history takes no arguments"
				} else {
					r.Response = strings.Join(commandBuf, "\n")

				}
				return r
			},
			Helpshort: "show the command history",
			Helplong: `
	Usage: history 

Show the command history`,
			Record: false,
			Clear: func() error {
				return nil
			},
		},

		"clear": &command{
			Call: func(c cliCommand) cliResponse {
				var r cliResponse
				if len(c.Args) != 1 {
					return cliResponse{
						Error: "clear takes one argument",
					}
				}
				cc := c.Args[0]
				if cliCommands[cc] == nil {
					e := fmt.Sprintf("invalid command: %v", cc)
					r.Error = e
				} else {
					e := cliCommands[cc].Clear()
					if e != nil {
						r.Error = e.Error()
					}
				}
				return r
			},
			Helpshort: "restore a variable to its default state",
			Helplong: `
	Usage: clear <command>

Restore a variable to its default state or clears it. For example:

	clear net

will clear the list of associated networks.`,
			Record: true,
			Clear: func() error {
				return fmt.Errorf("it's unclear how to clear clear")
			},
		},

		"help": &command{
			Call: func(c cliCommand) cliResponse {
				r := cliResponse{}
				if len(c.Args) == 0 { // display help on help, and list the short helps
					r.Response = "Display help on a command. Here is a list of commands:\n"
					var sortedNames []string
					for c, _ := range cliCommands {
						sortedNames = append(sortedNames, c)
					}
					sort.Strings(sortedNames)
					w := new(tabwriter.Writer)
					buf := bytes.NewBufferString(r.Response)
					w.Init(buf, 0, 8, 0, '\t', 0)
					for _, c := range sortedNames {
						fmt.Fprintln(w, c, "\t", ":\t", cliCommands[c].Helpshort, "\t")
					}
					w.Flush()
					r.Response = buf.String()
				} else if len(c.Args) == 1 { // try to display help on args[0]
					if cliCommands[c.Args[0]] != nil {
						r.Response = fmt.Sprintln(c.Args[0], ":", cliCommands[c.Args[0]].Helpshort)
						r.Response += fmt.Sprintln(cliCommands[c.Args[0]].Helplong)
					} else {
						e := fmt.Sprintf("no help on command: %v", c.Args[0])
						r.Error = e
					}
				} else {
					r.Error = "help takes one argument"
				}
				return r
			},
			Helpshort: "show this help message",
			Helplong: `
	Usage: help [command]

Show help on a command. If called with no arguments, show a summary of all
commands.`,
			Record: false,
			Clear: func() error {
				return nil
			},
		},

		"host_tap": &command{
			Call:      cliHostTap,
			Helpshort: "control host taps for communicating between hosts and VMs",
			Helplong: `
	Usage: host_tap [<create [bridge] vlan <A.B.C.D/MASK,dhcp,none>, delete <tap name>]

Control host taps on a named vlan for communicating between a host and any VMs
on that vlan. 

Calling host_tap with no arguments will list all created host_taps.

To create a host_tap on a particular vlan, invoke host_tap with the create
command:

	host_tap create <vlan> <ip/dhcp>

For example, to create a host tap with ip and netmask 10.0.0.1/24 on VLAN 5:

	host_tap create 5 10.0.0.1/24

Optionally, you can specify the bridge to create the host tap on:

	host_tap create <bridge> <vlan> <ip/dhcp>

Additionally, you can bring the tap up with DHCP by using "dhcp" instead of a
ip/netmask:

	host_tap create 5 dhcp

To delete a host tap, use the delete command and tap name from the host_tap list:

	host_tap delete <id>
	
To delete all host taps, use id -1, or 'clear host_tap':
	
	host_tap delete -1`,
			Record: true,
			Clear: func() error {
				resp := hostTapDelete("-1")
				if resp.Error == "" {
					return nil
				}
				return fmt.Errorf("%v", resp.Error)
			},
		},

		"mesh_degree": &command{
			Call:      meshageDegree,
			Helpshort: "view or set the current degree for this mesh node",
			Helplong: `
	Usage: mesh_degree [degree]

View or set the current degree for this mesh node.`,
			Record: true,
			Clear: func() error {
				meshageNode.SetDegree(0)
				return nil
			},
		},

		"mesh_dial": &command{
			Call:      meshageDial,
			Helpshort: "connect this node to another",
			Helplong: `
	Usage: mesh_dial <hostname>

Attempt to connect to another listening node.`,
			Record: true,
			Clear: func() error {
				return nil
			},
		},

		"mesh_dot": &command{
			Call:      meshageDot,
			Helpshort: "output a graphviz formatted dot file",
			Helplong: `
	Usage: mesh_dot <filename>

Output a graphviz formatted dot file representing the connected topology.`,
			Record: true,
			Clear: func() error {
				return nil
			},
		},

		"mesh_status": &command{
			Call:      meshageStatus,
			Helpshort: "display a short status report of the mesh",
			Helplong: `
	Usage: mesh_status

Display a short status report of the mesh.`,
			Record: false,
			Clear: func() error {
				return nil
			},
		},

		"mesh_list": &command{
			Call:      meshageList,
			Helpshort: "display the mesh adjacency list",
			Helplong: `
	Usage: mesh_list 

Display the mesh adjacency list.`,
			Record: false,
			Clear: func() error {
				return nil
			},
		},

		"mesh_hangup": &command{
			Call:      meshageHangup,
			Helpshort: "disconnect from a client",
			Helplong: `
	Usage: mesh_hangup <hostname>

Disconnect from a client.`,
			Record: true,
			Clear: func() error {
				return nil
			},
		},

		"mesh_msa_timeout": &command{
			Call:      meshageMSATimeout,
			Helpshort: "view or set the MSA timeout",
			Helplong: `
	Usage: mesh_msa_timeout [timeout]

View or the the Meshage State Announcement timeout.`,
			Record: true,
			Clear: func() error {
				meshageNode.SetMSATimeout(60)
				return nil
			},
		},

		"mesh_timeout": &command{
			Call:      meshageTimeoutCLI,
			Helpshort: "view or set the mesh timeout",
			Helplong: `
	Usage: mesh_timeout [timeout]

View or set the timeout on sending mesh commands.

When a mesh command is issued, if a response isn't sent within mesh_timeout
seconds, the command will be dropped and any future response will be discarded.
Note that this does not cancel the outstanding command - the node receiving the
command may still complete - but rather this node will stop waiting on a
response.`,
			Record: true,
			Clear: func() error {
				meshageTimeout = time.Duration(MESH_TIMEOUT_DEFAULT) * time.Second
				return nil
			},
		},

		"mesh_set": &command{
			Call:      meshageSet,
			Helpshort: "send a command to one or more connected clients",
			Helplong: `
	Usage: mesh_set [annotate] <recipients> <command>

Send a command to one or more connected clients.
For example, to get the vm_info from nodes kn1 and kn2:

	mesh_set kn[1-2] vm_info
	
Optionally, you can annotate the output with the hostname of all responders by
prepending the keyword 'annotate' to the command:

	mesh_set annotate kn[1-2] vm_info`,
			Record: true,
			Clear: func() error {
				return nil
			},
		},

		"mesh_broadcast": &command{
			Call:      meshageBroadcast,
			Helpshort: "send a command to all connected clients",
			Helplong: `
	Usage: mesh_broadcast [annotate] <command>

Send a command to all connected clients.
For example, to get the vm_info from all nodes:

	mesh_broadcast vm_info

Optionally, you can annotate the output with the hostname of all responders by
prepending the keyword 'annotate' to the command:

	mesh_broadcast annotate vm_info`,
			Record: true,
			Clear: func() error {
				return nil
			},
		},

		"hostname": &command{
			Call: func(c cliCommand) cliResponse {
				host, err := os.Hostname()
				if err != nil {
					return cliResponse{
						Error: err.Error(),
					}
				}
				return cliResponse{
					Response: host,
				}
			},
			Helpshort: "return the hostname",
			Helplong: `
	Usage: hostname

Return the hostname.`,
			Record: true,
			Clear: func() error {
				return nil
			},
		},

		"dnsmasq": &command{
			Call:      dnsmasqCLI,
			Helpshort: "start a dhcp/dns server on a specified ip",
			Helplong: `
	Usage: dnsmasq [start [<listen address> <DHCP low range> <DHCP high range> [config file], config file], kill <id>]

Start a dhcp/dns server on a specified IP with a specified range.  For example,
to start a DHCP server on IP 10.0.0.1 serving the range 10.0.0.2 -
10.0.254.254:

	dnsmasq start 10.0.0.1 10.0.0.2 10.0.254.254

To start only a from a config file:

	dnsmasq start /path/to/config

To list running dnsmasq servers, invoke dnsmasq with no arguments.  To kill a
running dnsmasq server, specify its ID from the list of running servers. For
example, to kill dnsmasq server 2:

	dnsmasq kill 2

To kill all running dnsmasq servers, pass -1 as the ID:

	dnsmasq kill -1

dnsmasq will provide DNS service from the host, as well as from /etc/hosts. 
You can specify an additional config file for dnsmasq by providing a file as an
additional argument.

	dnsmasq start 10.0.0.1 10.0.0.2 10.0.254.254 /tmp/dnsmasq-extra.conf

NOTE: If specifying an additional config file, you must provide the full path to
the file.`,
			Record: true,
			Clear: func() error {
				return nil
			},
		},

		"shell": &command{
			Call:      shellCLI,
			Helpshort: "execute a command",
			Helplong: `
	Usage: shell <command>

Execute a command under the credentials of the running user. 

Commands run until they complete or error, so take care not to execute a command
that does not return.`,
			Record: true,
			Clear: func() error {
				return nil
			},
		},

		"background": &command{
			Call:      backgroundCLI,
			Helpshort: "execute a command in the background",
			Helplong: `
	Usage: background <command>

Execute a command under the credentials of the running user. 

Commands run in the background and control returns immediately. Any output is
logged.`,
			Record: true,
			Clear: func() error {
				return nil
			},
		},

		"host_stats": &command{
			Call:      hostStatsCLI,
			Helpshort: "report statistics about the host",
			Helplong: `
	Usage: host_stats

Report statistics about the host including hostname, load averages, total and
free memory, and current bandwidth usage.

To output host statistics without the header, use the quiet argument:
	host_stats quiet`,
			Record: true,
			Clear: func() error {
				return nil
			},
		},

		"vm_snapshot": &command{
			Call:      cliVMSnapshot,
			Helpshort: "enable or disable snapshot mode when using disk images",
			Helplong: `
	Usage: vm_snapshot [true,false]

Enable or disable snapshot mode when using disk images. When enabled, disks
images will be loaded in memory when run and changes will not be saved. This
allows a single disk image to be used for many VMs.`,
			Record: true,
			Clear: func() error {
				info.Snapshot = true
				return nil
			},
		},

		"optimize": &command{
			Call:      optimizeCLI,
			Helpshort: "enable or disable several virtualization optimizations",
			Helplong: `
	Usage: optimize [ksm [true,false], hugepages [path], affinity [true,false]]

Enable or disable several virtualization optimizations, including Kernel
Samepage Merging, CPU affinity for VMs, and the use of hugepages.

To enable/disable Kernel Samepage Merging (KSM):
	optimize ksm [true,false]

To enable hugepage support:
	optimize hugepages </path/to/hugepages_mount>

To disable hugepage support:
	optimize hugepages ""

To enable/disable CPU affinity support:
	optimize affinity [true,false]

To set a CPU set filter for the affinity scheduler, for example (to use only
CPUs 1, 2-20):
	optimize affinity filter [1,2-20]

To clear a CPU set filter:
	optimize affinity filter

To view current CPU affinity mappings:
	optimize affinity

To disable all optimizations
	clear optimize`,
			Record: true,
			Clear: func() error {
				clearOptimize()
				return nil
			},
		},

		"version": &command{
			Call: func(c cliCommand) cliResponse {
				return cliResponse{
					Response: fmt.Sprintf("minimega %v %v", version.Revision, version.Date),
				}
			},
			Helpshort: "display the version",
			Helplong: `
	Usage: version

Display the version.`,
			Record: true,
			Clear: func() error {
				return nil
			},
		},

		"vm_config": &command{
			Call:      cliVMConfig,
			Helpshort: "display, save, or restore the current VM configuration",
			Helplong: `
	Usage: vm_config [save <name>, restore <name>]

Display, save, or restore the current VM configuration.

To display the current configuration, call vm_config with no arguments. 

List the current saved configurations with 'vm_config show'

To save a configuration:

	vm_config save <config name>

To restore a configuration:

	vm_config restore <config name>

Calling clear vm_config will clear all VM configuration options, but will not
remove saved configurations.`,
			Record: true,
			Clear:  cliClearVMConfig,
		},

		"debug": &command{
			Call:      cliDebug,
			Helpshort: "display internal debug information",
			Helplong: `
	Usage: debug [panic]

Display internal debug information. Invoking with the 'panic' keyword will
force minimega to dump a stacktrace upon crash or exit.`,
			Record: false,
			Clear: func() error {
				return nil
			},
		},

		"bridge_info": &command{
			Call:      cliBridgeInfo,
			Helpshort: "display information about virtual bridges",
			Helplong: `
	Usage: bridge_info

Display information about virtual bridges.`,
			Record: false,
			Clear: func() error {
				return nil
			},
		},

		"vm_flush": &command{
			Call:      cliVMFlush,
			Helpshort: "discard information about quit or failed VMs",
			Helplong: `
	Usage: vm_flush 

Discard information about VMs that have either quit or encountered an error.
This will remove any VMs with a state of "quit" or "error" from vm_info. Names
of VMs that have been flushed may be reused.`,
			Record: true,
			Clear: func() error {
				return nil
			},
		},

		"file": &command{
			Call:      cliFile,
			Helpshort: "work with files served by minimega",
			Helplong: `
	Usage: file <list [path], get <file>, delete <file>, status>
file allows you to transfer and manage files served by minimega in the
directory set by the -filepath flag (default is 'base'/files).

To list files currently being served, issue the list command with a directory
relative to the served directory:

	file list /foo

Issuing "file list /" will list the contents of the served directory.

Files can be deleted with the delete command:

	file delete /foo

If a directory is given, the directory will be recursively deleted.

Files are transferred using the get command. When a get command is issued, the
node will begin searching for a file matching the path and name within the
mesh. If the file exists, it will be transferred to the requesting node. If
multiple different files exist with the same name, the behavior is undefined.
When a file transfer begins, control will return to minimega while the
transfer completes.

To see files that are currently being transferred, use the status command:

	file status`,
			Record: true,
			Clear: func() error {
				return nil
			},
		},

		"viz": &command{
			Call:      cliDot,
			Helpshort: "visualize the current experiment as a graph",
			Helplong: `
	Usage: viz <filename>

Output the current experiment topology as a graphviz readable 'dot' file.`,
			Record: true,
			Clear: func() error {
				return nil
			},
		},

		"vyatta": &command{
			Call:      cliVyatta,
			Helpshort: "define vyatta configuration images",
			Helplong: `
	Usage: vyatta
>> vyatta dhcp add <network> <listen address> <DHCP low range> <DHCP high range>
>> vyatta dhcp delete <network>
>> vyatta interfaces <A.B.C.D/MASK,dhcp,none>[,<A.B.C.D/MASK,dhcp,none>...]
>> vyatta interfaces6 <IPv6 address/MASK,none>[,<IPv6 address/MASK,none>...]
>> vyatta rad <prefix>[,<prefix>...]
>> vyatta ospf <network>[,<network>...]
>> vyatta ospf3 <interface>[,<interface>...]
>> vyatta routes <network>,<next-hop>[ <network>,<next-hop> ...]
>> vyatta config <filename>
>> vyatta write [filename]

Define and write out vyatta router floppy disk images. 

vyatta takes a number of subcommands: 

- 'dhcp': Add DHCP service to a particular network by specifying the
network, default gateway, and start and stop addresses. For example, to
serve dhcp on 10.0.0.0/24, with a default gateway of 10.0.0.1:
	
	vyatta dhcp add 10.0.0.0/24 10.0.0.1 10.0.0.2 10.0.0.254

An optional DNS argument can be used to override the
nameserver. For example, to do the same as above with a
nameserver of 8.8.8.8:

	vyatta dhcp add 10.0.0.0/24 10.0.0.1 10.0.0.2 10.0.0.254 8.8.8.8

- 'interfaces': Add IPv4 addresses using CIDR notation. Optionally,
'dhcp' or 'none' may be specified. The order specified matches the
order of VLANs used in vm_net. This number of arguments must either be
0 or equal to the number of arguments in 'interfaces6' For example:

	vyatta interfaces 10.0.0.1/24 dhcp

- 'interfaces6': Add IPv6 addresses similar to 'interfaces'. The number
of arguments must either be 0 or equal to the number of arguments in
'interfaces'.

- 'rad': Enable router advertisements for IPv6. Valid arguments are IPv6
prefixes or "none". Order matches that of interfaces6. For example:

	vyatta rad 2001::/64 2002::/64

- 'ospf': Route networks using OSPF. For example:

	vyatta ospf 10.0.0.0/24 12.0.0.0/24

- 'ospf3': Route IPv6 interfaces using OSPF3. For example:

	vyatta ospf3 eth0 eth1

- 'routes': Set static routes. Routes are specified as

	<network>,<next-hop> ... 

For example: 
	
	vyatta routes 2001::0/64,123::1 10.0.0.0/24,12.0.0.1

- 'config': Override all other options and use a specified file as the
config file. For example: vyatta config /tmp/myconfig.boot

- 'write': Write the current configuration to file. If a filename is
omitted, a random filename will be used and the file placed in the path
specified by the -filepath flag. The filename will be returned.`,
			Record: true,
			Clear:  cliVyattaClear,
		},

		"vm_hotplug": &command{
			Call:      cliVMHotplug,
			Helpshort: "add and remove USB drives",
			Helplong: `
	Usage: vm_hotplug [add <id> <filename>, remove <id> <file id>]

Add and remove USB drives to a launched VM. 

To view currently attached media, call vm_hotplug with the 'show' argument and
a VM ID or name. To add a device, use the 'add' argument followed by the VM ID
or name, and the name of the file to add. For example, to add foo.img to VM 5:

	vm_hotplug add 5 foo.img

The add command will assign a disk ID, shown in vm_hotplug show. To remove
media, use the 'remove' argument with the VM ID and the disk ID. For example,
to remove the drive added above, named 0:

	vm_hotplug remove 5 0

To remove all hotplug devices, use ID -1.`,
			Record: true,
			Clear: func() error {
				return nil
			},
		},

		"vm_netmod": &command{
			Call:      cliVMNetMod,
			Helpshort: "disconnect or move network connections",
			Helplong: `
	Usage: vm_netmod <vm name or id> <tap position> <vlan,disconnect>

Disconnect or move existing network connections on a running VM. 

Network connections are indicated by their position in vm_net (same order in
vm_info) and are zero indexed. For example, to disconnect the first network
connection from a VM with 4 network connections:

	vm_netmod <vm name or id> 0 disconnect

To disconnect the second connection:

	vm_netmod <vm name or id> 1 disconnect

To move a connection, specify the new VLAN tag:

	vm_netmod <vm name or id> 0 100`,
			Record: true,
			Clear: func() error {
				return nil
			},
		},

		"vm_inject": &command{
			Call:      cliVMInject,
			Helpshort: "inject files into a qcow image",
			Helplong: `
	Usage: vm_inject <src qcow image>[:<partition>] [<dst qcow image name>] <src file1>:<dst file1> [<src file2>:<dst file2> ...]

Create a backed snapshot of a qcow2 image and injects one or more files into
the new snapshot.

src qcow image - the name of the qcow to use as the backing image file.

partition - The optional partition number in which the files should be
injected. Partition defaults to 1, but if multiple partitions exist and
partition is not explicitly specified, an error is thrown and files are not
injected.

dst qcow image name - The optional name of the snapshot image. This should be a
name only, if any extra path is specified, an error is thrown. This file will
be created at 'base'/files. A filename will be generated if this optional
parameter is omitted.

src file - The local file that should be injected onto the new qcow2 snapshot.

dst file - The path where src file should be injected in the new qcow2 snapshot.

If the src file or dst file contains spaces, use double quotes (" ") as in the
following example:

	vm_inject src.qc2 dst.qc2 "my file":"Program Files/my file"

Alternatively, when given a single argument, this command supplies the name of
the backing qcow image for a snapshot image.`,
			Record: true,
			Clear: func() error {
				return nil
			},
		},

		"define": &command{
			Call:      cliDefine,
			Helpshort: "define macros",
			Helplong: `
	Usage: define [macro[(<var1>[,<var2>...])] <command>]

Define literal and function like macros.

Macro keywords are in the form [a-zA-z0-9]+. When defining a macro, all text after the key is the macro expansion. For example:

	define key foo bar

Will replace "key" with "foo bar" in all command line arguments.

You can also specify function like macros in a similar way to function like macros in C. For example:

	define key(x,y) this is my x, this is my y

Will replace all instances of x and y in the expansion with the variable arguments. When used:

	key(foo,bar)

Will expand to:

	this is mbar foo, this is mbar bar

To show defined macros, invoke define with no arguments.`,
			Record: true,
			Clear: func() error {
				macro = gomacro.NewMacro()
				return nil
			},
		},

		"undefine": &command{
			Call:      cliUndefine,
			Helpshort: "undefine macros",
			Helplong: `
	Usage: undefine <macro>

Undefine macros by name.`,
			Record: true,
			Clear: func() error {
				return nil
			},
		},

		"echo": &command{
			Call: func(c cliCommand) cliResponse {
				return cliResponse{
					Response: strings.Join(c.Args, " "),
				}
			},
			Helpshort: "display a line of text",
			Helplong: `
	Usage: echo [<string>]

Return the command after macro expansion and comment removal.`,
			Record: true,
			Clear: func() error {
				return nil
			},
		},

		"vm_qmp": &command{
			Call:      cliVMQMP,
			Helpshort: "issue a JSON-encoded QMP command",
			Helplong: `
Issue a JSON-encoded QMP command. This is a convenience function for accessing
the QMP socket of a VM via minimega. vm_qmp takes two arguments, a VM ID or
name, and a JSON string, and returns the JSON encoded response. For example:

minimega$ vm_qmp 0 { "execute": "query-status" }
{"return":{"running":false,"singlestep":false,"status":"prelaunch"}}`,
			Record: true,
			Clear: func() error {
				return nil
			},
		},
	}
}

func (c cliCommand) String() string {
	args := strings.Join(c.Args, " ")
	return c.Command + " " + args
}

func cliDefine(c cliCommand) cliResponse {
	m := macro.List()
	if len(m) == 0 {
		return cliResponse{}
	}

	switch len(c.Args) {
	case 0:
		// create output
		var o bytes.Buffer
		w := new(tabwriter.Writer)
		w.Init(&o, 5, 0, 1, ' ', 0)
		fmt.Fprintln(&o, "macro\texpansion")
		for _, v := range m {
			k, e := macro.Macro(v)
			fmt.Fprintf(&o, "%v\t%v\n", k, e)
		}
		w.Flush()
		return cliResponse{
			Response: o.String(),
		}
	case 1:
		return cliResponse{
			Error: "define requires at least 2 arguments",
		}
	default:
		err := macro.Define(c.Args[0], strings.Join(c.Args[1:], " "))
		if err != nil {
			return cliResponse{
				Error: err.Error(),
			}
		}
	}
	return cliResponse{}
}

func cliUndefine(c cliCommand) cliResponse {
	if len(c.Args) != 1 {
		return cliResponse{
			Error: "undefine takes exactly one argument",
		}
	}
	log.Debug("undefine %v", c.Args[0])
	macro.Undefine(c.Args[0])
	return cliResponse{}
}

func makeCommand(s string) cliCommand {
	// macro expansion
	// special case - don't expand 'define' or 'undefine'
	var input string
	f := strings.Fields(s)
	if len(f) > 0 {
		if f[0] != "define" && f[0] != "undefine" {
			input = macro.Parse(s)
		} else {
			input = s
		}
	}
	log.Debug("macro expansion %v -> %v", s, input)
	f = strings.Fields(input)
	var command string
	var args []string
	if len(f) > 0 {
		command = f[0]
	}
	if len(f) > 1 {
		args = f[1:]
	}
	return cliCommand{
		Command: command,
		Args:    args,
	}
}

// local command line interface, wrapping readline
func cli() {
	for {
		prompt := "minimega$ "
		line, err := goreadline.Rlwrap(prompt)
		if err != nil {
			break // EOF
		}
		log.Debug("got from stdin:", line)

		c := makeCommand(string(line))

		commandChanLocal <- c
		for {
			r := <-ackChanLocal
			if r.Error != "" {
				log.Errorln(r.Error)
			}
			if r.Response != "" {
				if strings.HasSuffix(r.Response, "\n") {
					fmt.Print(r.Response)
				} else {
					fmt.Println(r.Response)
				}
			}
			if !r.More {
				log.Debugln("got last message")
				break
			} else {
				log.Debugln("expecting more data")
			}
		}
	}
}

func cliMux() {
	for {
		select {
		case c := <-commandChanLocal:
			c.ackChan = ackChanLocal
			ackChanLocal <- cliExec(c)
		case c := <-commandChanSocket:
			c.ackChan = ackChanSocket
			ackChanSocket <- cliExec(c)
		case c := <-commandChanMeshage:
			c.ackChan = ackChanMeshage
			ackChanMeshage <- cliExec(c)
		}
	}
}

// process commands from the command channel. each command is acknowledged with
// true/false success codes on commandAck.
func cliExec(c cliCommand) cliResponse {
	if c.Command == "" {
		return cliResponse{}
	}

	// super special case
	if c.Command == "vm_vince" {
		log.Fatalln(poeticDeath)
	}

	// special case, comments. Any line starting with # is a comment and WILL be
	// recorded.
	if strings.HasPrefix(c.Command, "#") {
		log.Debugln("comment:", c.Command, c.Args)
		s := c.Command
		if len(c.Args) > 0 {
			s += " " + strings.Join(c.Args, " ")
		}
		commandBuf = append(commandBuf, s)
		return cliResponse{}
	}

	if cliCommands[c.Command] == nil {
		e := fmt.Sprintf("invalid command: %v", c.Command)
		return cliResponse{
			Error: e,
		}
	}
	r := cliCommands[c.Command].Call(c)
	if r.Error == "" {
		if cliCommands[c.Command].Record {
			s := c.Command
			if len(c.Args) > 0 {
				s += " " + strings.Join(c.Args, " ")
			}
			commandBuf = append(commandBuf, s)
		}
	}
	return r
}

// sort and walk the api, emitting markdown for each entry
func docGen() {
	var keys []string
	for k, _ := range cliCommands {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	fmt.Println("# minimega API")

	for _, k := range keys {
		fmt.Printf("<h2 id=%v>%v</h2>\n", k, k)
		fmt.Println(cliCommands[k].Helplong)
	}
}

var poeticDeath = `
Willst du immer weiterschweifen?
Sieh, das Gute liegt so nah.
Lerne nur das Glück ergreifen,
denn das Glück ist immer da.
`
