package online

import "testing"

func TestBlockedAddr(t *testing.T) {
	for _, a := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "[::1]", "::1", "fd00::1", "192.168.1.1", "100.64.0.1", "64:ff9b::a00:1", "2002:c0a8:101::1"} {
		if !blockedAddr(a) {
			t.Errorf("%s is not blocked", a)
		}
	}
	for _, a := range []string{"93.184.216.34", "2606:2800:220:1::1"} {
		if blockedAddr(a) {
			t.Errorf("%s is blocked", a)
		}
	}
}
