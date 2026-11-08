package rbac

import "testing"

func TestAllows(t *testing.T) {
	for _, tc := range []struct {
		held []Role
		want Role
		ok   bool
	}{
		{nil, Read, false},
		{[]Role{Read}, Read, true},
		{[]Role{Read}, Use, false},
		{[]Role{Use}, Read, true},
		{[]Role{Use}, Execute, false},
		{[]Role{Execute}, Read, true},
		{[]Role{Execute}, Admin, false},
		{[]Role{Admin}, Use, true},
		{[]Role{Admin}, Execute, true},
		{[]Role{Read, Execute}, Execute, true},
	} {
		if got := Allows(tc.held, tc.want); got != tc.ok {
			t.Errorf("Allows(%v, %s) = %t, want %t", tc.held, tc.want, got, tc.ok)
		}
	}
}

func TestValid(t *testing.T) {
	for _, tc := range []struct {
		t    ResourceType
		role Role
		ok   bool
	}{
		{"", SystemAdmin, true},
		{"", Auditor, true},
		{"", Admin, false},
		{Inventory, Use, true},
		{Inventory, Execute, false},
		{Inventory, SystemAdmin, false},
		{JobTemplate, Execute, true},
		{JobTemplate, Use, false},
		{"bogus", Read, false},
	} {
		if got := Valid(tc.t, tc.role); got != tc.ok {
			t.Errorf("Valid(%q, %s) = %t, want %t", tc.t, tc.role, got, tc.ok)
		}
	}
}
