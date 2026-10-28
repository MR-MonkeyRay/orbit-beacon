package egress

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
			d := Dialer{Resolver: resolverFunc(func(context.Context, string) ([]net.IPAddr, error) { return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil }), Dial: func(context.Context, string, string) (net.Conn, error) { called = true; return nil, nil }}
			if _, e := d.DialContext(context.Background(), "tcp", "rebind.example:443"); !errors.Is(e, ErrDenied) || called {
				t.Fatalf("IP %s bypassed: %v, dialed=%v", ip, e, called)
			}
		})
	}
	t.Run("IPv4-translated private address", func(t *testing.T) {
		d := Dialer{Resolver: resolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("::ffff:0:10.0.0.25")}}, nil
		}), Dial: func(context.Context, string, string) (net.Conn, error) {
			t.Fatal("IPv4-translated private address dialed")
			return nil, nil
		}}
		if _, e := d.DialContext(context.Background(), "tcp", "translated.example:443"); !errors.Is(e, ErrDenied) {
			t.Fatal(e)
		}
	})
	t.Run("literal pinning", func(t *testing.T) {
		lookups := 0
		d := Dialer{Resolver: resolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
			lookups++
			if lookups > 1 {
				return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
			}
			return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
		}), Dial: func(_ context.Context, _ string, address string) (net.Conn, error) {
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
		d := Dialer{Resolver: resolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("10.0.0.1")}}, nil
		}), Dial: func(context.Context, string, string) (net.Conn, error) {
			t.Fatal("mixed answer dialed")
			return nil, nil
		}}
		if _, e := d.DialContext(context.Background(), "tcp", "mixed.example:443"); !errors.Is(e, ErrDenied) {
			t.Fatal(e)
		}
	})
	t.Run("metadata stays blocked with opt in", func(t *testing.T) {
		for _, ip := range []string{"169.254.169.254", "fd00:ec2::254", "::ffff:169.254.169.254", "100.100.100.200",
			"169.254.170.2", "169.254.170.23", "fd00:ec2::23", "::ffff:0:a9fe:a9fe", "64:ff9b::a9fe:aa02"} {
			d := Dialer{AllowPrivate: true, Resolver: resolverFunc(func(context.Context, string) ([]net.IPAddr, error) { return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil }), Dial: func(context.Context, string, string) (net.Conn, error) {
				t.Fatalf("metadata %s dialed", ip)
				return nil, nil
			}}
			if _, e := d.DialContext(context.Background(), "tcp", "metadata.example:80"); !errors.Is(e, ErrDenied) {
				t.Fatalf("metadata %s: %v", ip, e)
			}
		}
	})
	t.Run("explicit opt in", func(t *testing.T) {
		called := false
		d := Dialer{AllowPrivate: true, Resolver: net.DefaultResolver, Dial: func(context.Context, string, string) (net.Conn, error) { called = true; return nil, errors.New("test") }}
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
					d := Dialer{AllowPrivate: allow, Resolver: resolverFunc(func(context.Context, string) ([]net.IPAddr, error) { return []net.IPAddr{{IP: ip}}, nil }), Dial: func(context.Context, string, string) (net.Conn, error) { called = true; return nil, nil }}
					_, err := d.DialContext(context.Background(), "tcp", "transition.example:443")
					denied := metadataIP(net.IP(v[:])) || !allow && (form.blockedPublic || forbiddenIP(net.IP(v[:])))
					if called == denied || denied && !errors.Is(err, ErrDenied) {
						t.Fatalf("called=%v err=%v want denied=%v", called, err, denied)
					}
				})
			}
		}
	}
}

func TestDenyLocalWithPrivateOptIn(t *testing.T) {
	for _, target := range []struct {
		ip     string
		denied bool
	}{
		{"10.0.0.1", false}, {"172.16.0.1", false}, {"192.168.1.1", false}, {"fd12::1", false}, {"8.8.8.8", false},
		{"127.0.0.1", true}, {"::1", true}, {"169.254.1.2", true}, {"fe80::1", true},
		{"0.0.0.0", true}, {"::", true}, {"224.0.0.1", true}, {"ff02::1", true},
		{"169.254.169.254", true}, {"100.100.100.200", true}, {"fd00:ec2::254", true},
		{"::ffff:127.0.0.1", true}, {"::ffff:0:127.0.0.1", true}, {"::127.0.0.1", true},
		{"64:ff9b::7f00:1", true}, {"64:ff9b:1:7f00:0:100::", true}, {"2002:7f00:1::1", true}, {"2001:0:808:808::80ff:fffe", true},
		{"::ffff:0:169.254.1.2", true}, {"64:ff9b::a9fe:102", true}, {"2002:a9fe:102::1", true},
		{"::ffff:0:169.254.169.254", true}, {"64:ff9b::a9fe:a9fe", true}, {"2002:6464:64c8::1", true},
	} {
		t.Run(target.ip, func(t *testing.T) {
			called := false
			d := Dialer{
				AllowPrivate: true,
				DenyLocal:    true,
				Resolver: resolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
					return []net.IPAddr{{IP: net.ParseIP(target.ip)}}, nil
				}),
				Dial: func(context.Context, string, string) (net.Conn, error) {
					called = true
					return nil, nil
				},
			}
			_, err := d.DialContext(context.Background(), "tcp", "backup.example:443")
			if called == target.denied || target.denied && !errors.Is(err, ErrDenied) {
				t.Fatalf("called=%v err=%v want denied=%v", called, err, target.denied)
			}
		})
	}
}

func TestDenyLocalMixedDNSFailsClosed(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "169.254.1.2", "64:ff9b::7f00:1", "100.100.100.200"} {
		t.Run(ip, func(t *testing.T) {
			d := Dialer{
				AllowPrivate: true,
				DenyLocal:    true,
				Resolver: resolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
					return []net.IPAddr{{IP: net.ParseIP("10.0.0.1")}, {IP: net.ParseIP(ip)}}, nil
				}),
				Dial: func(context.Context, string, string) (net.Conn, error) {
					t.Fatal("mixed allowed/denied answer dialed")
					return nil, nil
				},
			}
			if _, err := d.DialContext(context.Background(), "tcp", "mixed.example:443"); !errors.Is(err, ErrDenied) {
				t.Fatal(err)
			}
		})
	}
}

func TestDialPinsPrivateDNSAnswer(t *testing.T) {
	lookups, calls := 0, 0
	wantErr := errors.New("dial failed")
	d := Dialer{
		AllowPrivate: true,
		DenyLocal:    true,
		Resolver: resolverFunc(func(_ context.Context, host string) ([]net.IPAddr, error) {
			lookups++
			if host != "backup.example" {
				t.Fatalf("unexpected lookup %s", host)
			}
			return []net.IPAddr{{IP: net.ParseIP("10.0.0.1")}, {IP: net.ParseIP("fd12::1")}}, nil
		}),
		Dial: func(_ context.Context, network, address string) (net.Conn, error) {
			want := []string{"10.0.0.1:443", "[fd12::1]:443"}
			if network != "tcp" || calls >= len(want) || address != want[calls] {
				t.Fatalf("not dialing checked literal: %s %s", network, address)
			}
			calls++
			return nil, wantErr
		},
	}
	if _, err := d.DialContext(context.Background(), "tcp", "backup.example:443"); !errors.Is(err, wantErr) {
		t.Fatal(err)
	}
	if lookups != 1 || calls != 2 {
		t.Fatalf("lookups=%d calls=%d", lookups, calls)
	}
}
