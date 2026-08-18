package inventory

import (
	"strings"
)

// dmiInfo is the machine identity read from SMBIOS/DMI (or the platform
// equivalent). Every field is best-effort: an empty string means "this
// platform could not say", and the hardware snapshot omits it entirely so
// the server never records a false "no serial" belief.
type dmiInfo struct {
	Manufacturer string
	Model        string
	Serial       string
	Chassis      string
}

// chassisName maps an SMBIOS System Enclosure type code to a coarse,
// lowercase name. The full DMTF table distinguishes pizza boxes from lunch
// boxes; the fleet cares about a handful of buckets, so anything exotic
// collapses to "" rather than inventing a category downstream tools would
// have to special-case.
func chassisName(code int) string {
	switch code {
	case 3, 4, 5, 6, 7, 15, 16, 24, 35, 36:
		return "desktop"
	case 13:
		return "all-in-one"
	case 8, 9, 10, 14:
		return "laptop"
	case 31:
		return "convertible"
	case 32:
		return "detachable"
	case 11:
		return "handheld"
	case 30:
		return "tablet"
	case 17, 23, 25, 28, 29:
		return "server"
	case 34:
		return "embedded"
	default:
		return ""
	}
}

// dmiJunk holds placeholder strings OEMs ship instead of real values. They
// are compared case-insensitively after trimming. Recording them as identity
// would be worse than recording nothing: a thousand devices with serial
// "To Be Filled By O.E.M." collide in any system that matches on serial.
var dmiJunk = map[string]struct{}{
	"":                       {},
	"to be filled by o.e.m.": {},
	"to be filled by oem":    {},
	"default string":         {},
	"system serial number":   {},
	"system manufacturer":    {},
	"system product name":    {},
	"chassis serial number":  {},
	"none":                   {},
	"not specified":          {},
	"not applicable":         {},
	"no enclosure":           {},
	"unknown":                {},
	"oem":                    {},
	"0":                      {},
	"123456789":              {},
	"0123456789":             {},
	"empty":                  {},
}

// cleanDMI trims a raw DMI string and blanks known OEM placeholders.
func cleanDMI(s string) string {
	s = strings.TrimSpace(s)
	if _, junk := dmiJunk[strings.ToLower(s)]; junk {
		return ""
	}
	return s
}
