package dockerdb

import "testing"

// TestPortsAreUnique is the whole reason the list exists.
//
// Two suites on one port is not a test failure anybody can read: it is a
// connection to a database with the wrong schema in it, arriving as a missing
// column or a row that should not be there, in whichever suite happens to run
// second.
func TestPortsAreUnique(t *testing.T) {
	owner := make(map[int]string, len(ports))
	for name, port := range ports {
		if taken, ok := owner[port]; ok {
			t.Errorf("port %d is claimed by both %s and %s", port, taken, name)
			continue
		}
		owner[port] = name
	}
}

// TestPortsAreInTheReservedRange keeps the list from drifting into a range
// something else on the machine might reasonably want.
//
// 55432 is 5432 with a 5 in front of it, and everything else was allocated
// upward from there. Staying inside one block is what makes "is anything
// listening on 554xx?" a question with a useful answer.
func TestPortsAreInTheReservedRange(t *testing.T) {
	for name, port := range ports {
		if port < 55432 || port > 55499 {
			t.Errorf("%s is on %d, outside the 55432-55499 block", name, port)
		}
	}
}
