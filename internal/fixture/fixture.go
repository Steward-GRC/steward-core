// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

// Package fixture holds the sample data the tests in this repo use, from the
// Steward design brief. Nothing here is a real person, group or document.
package fixture

// People, by email.
const (
	AliceEmail = "alice@example.org" // site admin
	BobEmail   = "bob@example.org"   // author
)

// Directory groups.
const (
	FacilitiesTeam = "facilities-team"
	FinanceTeam    = "finance-team"
)

// Documents.
const (
	DeskBookingPolicy   = "Desk Booking Policy"
	ExpenseClaimsPolicy = "Expense Claims Policy"
	TravelProcedure     = "Travel Booking Procedure"
)

// DocAddress is an address from the documentation range.
const DocAddress = "192.0.2.10"
