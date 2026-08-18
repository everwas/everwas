package inventory

import "testing"

func TestChassisName(t *testing.T) {
	cases := []struct {
		code int
		want string
	}{
		{3, "desktop"},
		{7, "desktop"},  // tower
		{35, "desktop"}, // mini pc
		{9, "laptop"},
		{10, "laptop"}, // notebook
		{14, "laptop"}, // sub notebook
		{13, "all-in-one"},
		{31, "convertible"},
		{32, "detachable"},
		{30, "tablet"},
		{17, "server"}, // main server chassis
		{23, "server"}, // rack mount
		{28, "server"}, // blade
		{34, "embedded"},
		{1, ""},  // other
		{2, ""},  // unknown
		{12, ""}, // docking station is not the machine
		{99, ""}, // out of table
	}
	for _, c := range cases {
		if got := chassisName(c.code); got != c.want {
			t.Errorf("chassisName(%d) = %q, want %q", c.code, got, c.want)
		}
	}
}

func TestCleanDMIBlanksOEMJunk(t *testing.T) {
	junk := []string{
		"",
		"   ",
		"To Be Filled By O.E.M.",
		"to be filled by o.e.m.",
		"Default string",
		"System Serial Number",
		"System manufacturer",
		"System Product Name",
		"None",
		"Not Specified",
		"0",
		"123456789",
		"unknown",
	}
	for _, s := range junk {
		if got := cleanDMI(s); got != "" {
			t.Errorf("cleanDMI(%q) = %q, want empty", s, got)
		}
	}
}

func TestCleanDMIKeepsRealValues(t *testing.T) {
	cases := map[string]string{
		"  PF3K2ABC  ":       "PF3K2ABC",
		"LENOVO":             "LENOVO",
		"ThinkPad X1 Carbon": "ThinkPad X1 Carbon",
		"Dell Inc.":          "Dell Inc.",
	}
	for in, want := range cases {
		if got := cleanDMI(in); got != want {
			t.Errorf("cleanDMI(%q) = %q, want %q", in, got, want)
		}
	}
}
