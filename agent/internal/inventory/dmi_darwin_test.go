//go:build darwin

package inventory

import "testing"

func TestParseHardwareOverview(t *testing.T) {
	raw := []byte(`{
	  "SPHardwareDataType": [{
	    "machine_model": "MacBookPro18,3",
	    "machine_name": "MacBook Pro",
	    "serial_number": "C02XXXXXXXXX"
	  }]
	}`)
	got := parseHardwareOverview(raw)
	want := dmiInfo{
		Manufacturer: "Apple Inc.",
		Model:        "MacBookPro18,3",
		Serial:       "C02XXXXXXXXX",
		Chassis:      "laptop",
	}
	if got != want {
		t.Errorf("parseHardwareOverview = %+v, want %+v", got, want)
	}
}

func TestParseHardwareOverviewGarbage(t *testing.T) {
	if got := parseHardwareOverview([]byte("not json")); got != (dmiInfo{}) {
		t.Errorf("expected zero value on garbage, got %+v", got)
	}
	if got := parseHardwareOverview([]byte(`{"SPHardwareDataType": []}`)); got != (dmiInfo{}) {
		t.Errorf("expected zero value on empty array, got %+v", got)
	}
}

func TestAppleChassis(t *testing.T) {
	cases := map[string]string{
		"MacBook Pro": "laptop",
		"MacBook Air": "laptop",
		"Mac mini":    "desktop",
		"Mac Studio":  "desktop",
		"Mac Pro":     "desktop",
		"iMac":        "all-in-one",
		"":            "",
		"Xserve":      "",
	}
	for in, want := range cases {
		if got := appleChassis(in); got != want {
			t.Errorf("appleChassis(%q) = %q, want %q", in, got, want)
		}
	}
}
