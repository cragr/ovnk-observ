package nbcount

import "testing"

func TestParseExternalIDs(t *testing.T) {
	cases := []struct {
		name        string
		in          map[string]string
		owner, netw string
	}{
		{"typed", map[string]string{"k8s.ovn.org/owner-type": "NetworkPolicy", "k8s.ovn.org/owner-controller": "repro-vlan3126-network-controller"}, "NetworkPolicy", "repro-vlan3126"},
		{"default controller", map[string]string{"k8s.ovn.org/owner-controller": "default-network-controller"}, "none", "default"},
		{"empty map", map[string]string{}, "none", "default"},
		{"nil", nil, "none", "default"},
		{"no suffix", map[string]string{"k8s.ovn.org/owner-controller": "custom"}, "none", "custom"},
		{"empty controller", map[string]string{"k8s.ovn.org/owner-controller": ""}, "none", "default"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o, n := ParseExternalIDs(c.in)
			if o != c.owner || n != c.netw {
				t.Fatalf("got (%q,%q), want (%q,%q)", o, n, c.owner, c.netw)
			}
		})
	}
}

func TestParseUUID(t *testing.T) {
	s := "a5048458-9663-8742-9012-000000000000"
	u, err := ParseUUID(s)
	if err != nil {
		t.Fatal(err)
	}
	if u[0] != 0xa5 || u[15] != 0 || u[7] != 0x42 {
		t.Fatalf("bad bytes %x", u)
	}
	if got := FormatUUID(u); got != s {
		t.Fatalf("round trip %q", got)
	}
	for _, bad := range []string{"bad", "", "a5048458-9663-8742-9012-00000000000z", "a504845896638742901200000000000000"} {
		if _, err := ParseUUID(bad); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}
