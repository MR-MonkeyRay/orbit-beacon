package notify

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"testing"
)

type resolverFunc func(context.Context, string) ([]net.IPAddr, error)

func (f resolverFunc) LookupIPAddr(c context.Context, h string) ([]net.IPAddr, error) { return f(c, h) }
func TestDialRefusesPrivateAndRebinding(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "::1", "10.0.0.1", "172.16.0.1", "192.168.1.1", "169.254.169.254", "fd00:ec2::254", "fe80::1", "224.0.0.1", "ff02::1", "0.0.0.0", "::", "::ffff:127.0.0.1", "100.100.100.200", "64:ff9b::a00:1"} {
		t.Run(ip, func(t *testing.T) {
			called := false
			d := safeDialer{resolver: resolverFunc(func(context.Context, string) ([]net.IPAddr, error) { return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil }), dial: func(context.Context, string, string) (net.Conn, error) { called = true; return nil, nil }}
			if _, e := d.DialContext(context.Background(), "tcp", "rebind.example:443"); !errors.Is(e, errEgress) || called {
				t.Fatalf("IP %s bypassed: %v, dialed=%v", ip, e, called)
			}
		})
	}
	t.Run("IPv4-translated private address", func(t *testing.T) {
		d := safeDialer{resolver: resolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("::ffff:0:10.0.0.25")}}, nil
		}), dial: func(context.Context, string, string) (net.Conn, error) {
			t.Fatal("IPv4-translated private address dialed")
			return nil, nil
		}}
		if _, e := d.DialContext(context.Background(), "tcp", "translated.example:443"); !errors.Is(e, errEgress) {
			t.Fatal(e)
		}
	})
	t.Run("literal pinning", func(t *testing.T) {
		lookups := 0
		d := safeDialer{resolver: resolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
			lookups++
			if lookups > 1 {
				return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
			}
			return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
		}), dial: func(_ context.Context, _ string, address string) (net.Conn, error) {
			if address != "8.8.8.8:443" {
				t.Errorf("hostname redialed: %s", address)
			}
			return nil, errors.New("test dial")
		}}
		d.DialContext(context.Background(), "tcp", "rebind.example:443")
		if lookups != 1 {
			t.Fatal("resolved again")
		}
	})
	t.Run("mixed answer", func(t *testing.T) {
		d := safeDialer{resolver: resolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("10.0.0.1")}}, nil
		}), dial: func(context.Context, string, string) (net.Conn, error) {
			t.Fatal("mixed answer dialed")
			return nil, nil
		}}
		if _, e := d.DialContext(context.Background(), "tcp", "mixed.example:443"); !errors.Is(e, errEgress) {
			t.Fatal(e)
		}
	})
	t.Run("metadata stays blocked with opt in", func(t *testing.T) {
		for _, ip := range []string{"169.254.169.254", "fd00:ec2::254", "::ffff:169.254.169.254", "100.100.100.200",
			"169.254.170.2", "169.254.170.23", "fd00:ec2::23", "::ffff:0:a9fe:a9fe", "64:ff9b::a9fe:aa02"} {
			d := safeDialer{allowPrivate: true, resolver: resolverFunc(func(context.Context, string) ([]net.IPAddr, error) { return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil }), dial: func(context.Context, string, string) (net.Conn, error) {
				t.Fatalf("metadata %s dialed", ip)
				return nil, nil
			}}
			if _, e := d.DialContext(context.Background(), "tcp", "metadata.example:80"); !errors.Is(e, errEgress) {
				t.Fatalf("metadata %s: %v", ip, e)
			}
		}
	})
	t.Run("explicit opt in", func(t *testing.T) {
		called := false
		d := safeDialer{allowPrivate: true, resolver: net.DefaultResolver, dial: func(context.Context, string, string) (net.Conn, error) { called = true; return nil, errors.New("test") }}
		d.DialContext(context.Background(), "tcp", "127.0.0.1:443")
		if !called {
			t.Fatal("opt in ignored")
		}
	})
}

func TestTransitionAddresses(t *testing.T) {
	forms := []struct {
		name          string
		encode        func([4]byte) net.IP
		blockedPublic bool
	}{
		{"mapped", func(v [4]byte) net.IP { return net.ParseIP("::ffff:" + net.IP(v[:]).String()) }, false},
		{"compatible", func(v [4]byte) net.IP { return net.ParseIP("::" + net.IP(v[:]).String()) }, true},
		{"nat64-global", func(v [4]byte) net.IP {
			b := netip.MustParseAddr("64:ff9b::").As16()
			copy(b[12:], v[:])
			return net.IP(b[:])
		}, true},
		{"nat64-local-48", func(v [4]byte) net.IP {
			b := netip.MustParseAddr("64:ff9b:1::").As16()
			copy(b[6:8], v[:2])
			copy(b[9:11], v[2:])
			return net.IP(b[:])
		}, true},
		{"nat64-local-56", func(v [4]byte) net.IP {
			b := netip.MustParseAddr("64:ff9b:1::").As16()
			b[7] = v[0]
			copy(b[9:12], v[1:])
			return net.IP(b[:])
		}, true},
		{"nat64-local-64", func(v [4]byte) net.IP {
			b := netip.MustParseAddr("64:ff9b:1::").As16()
			copy(b[9:13], v[:])
			return net.IP(b[:])
		}, true},
		{"nat64-local-96", func(v [4]byte) net.IP {
			b := netip.MustParseAddr("64:ff9b:1::").As16()
			copy(b[12:], v[:])
			return net.IP(b[:])
		}, true},
		{"6to4", func(v [4]byte) net.IP {
			b := netip.MustParseAddr("2002::1").As16()
			copy(b[2:6], v[:])
			return net.IP(b[:])
		}, true},
		{"teredo", func(v [4]byte) net.IP {
			b := netip.MustParseAddr("2001:0:808:808::").As16()
			for i, x := range v {
				b[12+i] = ^x
			}
			return net.IP(b[:])
		}, true},
	}
	for _, form := range forms {
		for _, target := range []string{"169.254.169.254", "100.100.100.200", "10.0.0.1", "127.0.0.1", "8.8.8.8"} {
			for _, allow := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/allow=%v", form.name, target, allow), func(t *testing.T) {
					v := netip.MustParseAddr(target).As4()
					ip := form.encode(v)
					candidates := embeddedIPv4(ip)
					found := false
					for _, candidate := range candidates {
						if candidate.Equal(net.IP(v[:])) {
							found = true
						}
					}
					if !found {
						t.Fatalf("embedded address not found in %s", ip)
					}
					called := false
					d := safeDialer{allowPrivate: allow, resolver: resolverFunc(func(context.Context, string) ([]net.IPAddr, error) { return []net.IPAddr{{IP: ip}}, nil }), dial: func(context.Context, string, string) (net.Conn, error) { called = true; return nil, nil }}
					_, err := d.DialContext(context.Background(), "tcp", "transition.example:443")
					denied := metadataIP(net.IP(v[:])) || !allow && (form.blockedPublic || forbiddenIP(net.IP(v[:])))
					if called == denied || denied && !errors.Is(err, errEgress) {
						t.Fatalf("called=%v err=%v want denied=%v", called, err, denied)
					}
				})
			}
		}
	}
}
