// Copyright (2013) Sandia Corporation.
// Under the terms of Contract DE-AC04-94AL85000 with Sandia Corporation,
// the U.S. Government retains certain rights in this software.

package main

import (
	"fmt"
	"io"
	log "minilog"
	"os"
	"path/filepath"
	"ranges"
	"strconv"
	"strings"
	"time"
)

var cmdSub = &Command{
	UsageLine: "sub -r <reservation name> -k <kernel path> -i <initrd path> {-n <integer> | -w <node list>} [OPTIONS]",
	Short:     "create a reservation",
	Long: `
Create a new reservation.

REQUIRED FLAGS:

The -r flag sets the name for the reservation.

The -k flag gives the location of the kernel the nodes should boot. This
kernel will be copied to a separate directory for use.

The -i flag gives the location of the initrd the nodes should boot. This
file will be copied to a separate directory for use.

The -profile flag gives the name of a Cobbler profile the nodes should
boot. This flag takes precedence over the -k and -i flags.

The -n flag indicates that the specified number of nodes should be
included in the reservation. The first available nodes will be allocated.

OPTIONAL FLAGS:

The -c flag sets any kernel command line arguments. (eg "console=tty0").

The -t flag is used to specify the reservation time. Time denominations should be specified in days(d), hours(h), and minutes(m), in that order. Days are defined as 24*60 minutes. Example: To make a reservation for 7 days: -t 7d. To make a reservation for 4 days, 6 hours, 30 minutes: -t 4d6h30m (default = 60m)

The -s flag is a boolean to enable 'speculative' mode; this will print a selection of available times for the reservation, but will not actually make the reservation. Intended to be used with the -a flag to select a specific time slot.

The -a flag indicates that the reservation should take place on or after the specified time, given in the format "2017-Jan-2-15:04". Especially useful in conjunction with the -s flag.
	`,
}

var subR string       // -r flag
var subK string       // -k flag
var subI string       // -i
var subN int          // -n
var subC string       // -c
var subT string       // -t
var subS bool         // -s
var subA string       // -a
var subW string       // -w
var subProfile string // -profile

func init() {
	// break init cycle
	cmdSub.Run = runSub

	cmdSub.Flag.StringVar(&subR, "r", "", "")
	cmdSub.Flag.StringVar(&subK, "k", "", "")
	cmdSub.Flag.StringVar(&subI, "i", "", "")
	cmdSub.Flag.IntVar(&subN, "n", 0, "")
	cmdSub.Flag.StringVar(&subC, "c", "", "")
	cmdSub.Flag.StringVar(&subT, "t", "60m", "")
	cmdSub.Flag.BoolVar(&subS, "s", false, "")
	cmdSub.Flag.StringVar(&subA, "a", "", "")
	cmdSub.Flag.StringVar(&subW, "w", "", "")
	cmdSub.Flag.StringVar(&subProfile, "profile", "", "")
}

func runSub(cmd *Command, args []string) {
	var nodes []string          // if the user has requested specific nodes
	var reservation Reservation // the new reservation
	var newSched []TimeSlice    // the new schedule
	format := "2006-Jan-2-15:04"

	// parse duration requested
	var days int = 0
	duration := 0
	nanoseconds, err := time.ParseDuration(subT)
	if err != nil {
		// Check for a number of days in the argument
		if dInd := strings.Index(subT, "d"); dInd >= 0 {
			days, err = strconv.Atoi(subT[:dInd])
			if err == nil {
				duration = days * 24 * 60 //convert to minutes
				subT = subT[dInd+1:]      //remove days from string
				if subT != "" {           // capture any additional time
					nanoseconds, err = time.ParseDuration(subT)
				}
			}
		}
		if err != nil {
			log.Fatal("Unable to parse -t argument: %v\n", err)
		}
	}
	log.Debug("duration: %v, nano: %v\n", duration, nanoseconds/time.Minute)
	duration = duration + int(nanoseconds/time.Minute)
	if duration < MINUTES_PER_SLICE { //1 slice minimum reservation time
		duration = MINUTES_PER_SLICE
	}

	// validate arguments
	if subR == "" || (subN == 0 && subW == "") {
		help([]string{"sub"})
		log.Fatalln("Missing required argument")
	}

	if (subK == "" || subI == "") && subProfile == "" {
		help([]string{"sub"})
		log.Fatalln("Must specify either a kernel & initrd, or a Cobbler profile")
	}

	if subProfile != "" && !igorConfig.UseCobbler {
		log.Fatalln("igor is not configured to use Cobbler, cannot specify a Cobbler profile")
	}

	// Validate the cobbler profile
	if subProfile != "" {
		cobblerProfiles, err := processWrapper("cobbler", "profile", "list")
		if err != nil {
			log.Fatal("couldn't get list of cobbler profiles: %v\n", err)
		}
		if !strings.Contains(cobblerProfiles, subProfile) {
			log.Fatal("Cobbler profile does not exist: ", subProfile)
		}
	}

	user, err := getUser()
	if err != nil {
		log.Fatalln("cannot determine current user", err)
	}

	// Make sure there's not already a reservation with this name
	for _, r := range Reservations {
		if r.ResName == subR {
			log.Fatalln("A reservation named ", subR, " already exists.")
		}
	}

	// figure out which nodes to reserve
	if subW != "" {
		rnge, _ := ranges.NewRange(igorConfig.Prefix, igorConfig.Start, igorConfig.End)
		nodes, _ = rnge.SplitRange(subW)
		if len(nodes) == 0 {
			log.Fatal("Couldn't parse node specification %v\n", subW)
		}
	}

	// Make sure the reservation doesn't exceed any limits
	if user.Username != "root" && igorConfig.NodeLimit > 0 {
		if subN > igorConfig.NodeLimit || len(nodes) > igorConfig.NodeLimit {
			log.Fatal("Only root can make a reservation of more than %v nodes", igorConfig.NodeLimit)
		}
	}
	if user.Username != "root" && igorConfig.TimeLimit > 0 {
		if duration > igorConfig.TimeLimit {
			log.Fatal("Only root can make a reservation longer than %v minutes", igorConfig.TimeLimit)
		}
	}

	when := time.Now().Add(-time.Minute * MINUTES_PER_SLICE) //keep from putting the reservation 1 minute into future
	if subA != "" {
		loc, _ := time.LoadLocation("Local")
		t, _ := time.Parse(format, subA)
		when = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, loc)
	}

	// If this is a speculative call, run findReservationAfter a few times,
	// print, and exit
	if subS {
		fmt.Println("AVAILABLE RESERVATIONS")
		fmt.Println("START\t\t\tEND")
		for i := 0; i < 10; i++ {
			var r Reservation
			if subN > 0 {
				r, _, err = findReservationAfter(duration, subN, when.Add(time.Duration(i*10)*time.Minute).Unix())
				if err != nil {
					log.Fatalln(err)
				}
			} else if subW != "" {
				r, _, err = findReservationGeneric(duration, 0, nodes, true, when.Add(time.Duration(i*10)*time.Minute).Unix())
				if err != nil {
					log.Fatalln(err)
				}
			}
			fmt.Printf("%v\t%v\n", time.Unix(r.StartTime, 0).Format(format), time.Unix(r.EndTime, 0).Format(format))
		}
		return
	}

	if subW != "" {
		if subN > 0 {
			log.Fatalln("Both -n and -w options used. Operation canceled.")
		}
		reservation, newSched, err = findReservationGeneric(duration, 0, nodes, true, when.Unix())
	} else if subN > 0 {
		reservation, newSched, err = findReservationAfter(duration, subN, when.Unix())
	}
	if err != nil {
		log.Fatalln(err)
	}

	// pick a network segment
	var vlan int
VlanLoop:
	for vlan = igorConfig.VLANMin; vlan <= igorConfig.VLANMax; vlan++ {
		for _, r := range Reservations {
			if vlan == r.Vlan {
				continue VlanLoop
			}
		}
		break
	}
	if vlan > igorConfig.VLANMax {
		log.Fatal("couldn't assign a vlan!")
	}
	reservation.Vlan = vlan

	reservation.Owner = user.Username
	reservation.ResName = subR
	reservation.KernelArgs = subC
	reservation.CobblerProfile = subProfile // safe to do even if unset

	// Add it to the list of reservations
	Reservations[reservation.ID] = reservation

	// If we're not doing a Cobbler profile...
	if subProfile == "" {
		// copy kernel and initrd
		// 1. Validate and open source files
		ksource, err := os.Open(subK)
		if err != nil {
			log.Fatal("couldn't open kernel: %v", err)
		}
		isource, err := os.Open(subI)
		if err != nil {
			log.Fatal("couldn't open initrd: %v", err)
		}

		// make kernel copy
		fname := filepath.Join(igorConfig.TFTPRoot, "igor", subR+"-kernel")
		kdest, err := os.Create(fname)
		if err != nil {
			log.Fatal("failed to create %v -- %v", fname, err)
		}
		io.Copy(kdest, ksource)
		kdest.Close()
		ksource.Close()

		// make initrd copy
		fname = filepath.Join(igorConfig.TFTPRoot, "igor", subR+"-initrd")
		idest, err := os.Create(fname)
		if err != nil {
			log.Fatal("failed to create %v -- %v", fname, err)
		}
		io.Copy(idest, isource)
		idest.Close()
		isource.Close()
	}

	timefmt := "Jan 2 15:04"
	rnge, _ := ranges.NewRange(igorConfig.Prefix, igorConfig.Start, igorConfig.End)
	fmt.Printf("Reservation created for %v - %v\n", time.Unix(reservation.StartTime, 0).Format(timefmt), time.Unix(reservation.EndTime, 0).Format(timefmt))
	unsplit, _ := rnge.UnsplitRange(reservation.Hosts)
	fmt.Printf("Nodes: %v\n", unsplit)

	emitReservationLog("CREATED", reservation)

	Schedule = newSched

	// update the network config
	//err = networkSet(reservation.Hosts, vlan)
	//if err != nil {
	//	log.Fatal("error setting network isolation: %v", err)
	//}

	putReservations()
	putSchedule()
}
