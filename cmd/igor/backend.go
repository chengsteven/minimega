// Copyright (2018) Sandia Corporation.
// Under the terms of Contract DE-AC04-94AL85000 with Sandia Corporation,
// the U.S. Government retains certain rights in this software.

package main

import "log"

type Backend interface {
	// Install activates a reservation
	Install(*Reservation) error

	// Uninstall deactivates a reservation
	Uninstall(*Reservation) error
}

// MockBackend can be used for testing
type MockBackend struct{}

func (b *MockBackend) Install(r *Reservation) error {
	log.Printf("mock install %v", r.Name)
	return nil
}

func (b *MockBackend) Uninstall(r *Reservation) error {
	log.Printf("mock uninstall %v", r.Name)
	return nil
}
