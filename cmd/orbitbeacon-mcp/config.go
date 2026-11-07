package main

import (
	"errors"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
)

var tokenPattern = regexp.MustCompile(`^obt_[A-Za-z0-9_-]{43}$`)

type config struct{ baseURL, token string }

func loadConfig(getenv func(string) string) (config, error) {
	raw := getenv("ORBITBEACON_URL")
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.Host == "" || u.User != nil ||
		(u.Scheme != "http" && u.Scheme != "https") ||
		(u.Path != "" && u.Path != "/") || u.RawPath != "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return config{}, errors.New("ORBITBEACON_URL must be an HTTP(S) origin without path, query, fragment or user information")
	}
	host := u.Hostname()
	if host == "" {
		return config{}, errors.New("ORBITBEACON_URL requires a host")
	}
	if u.Scheme == "http" {
		loopback := strings.EqualFold(host, "localhost")
		if addr, err := netip.ParseAddr(host); err == nil {
			loopback = addr.Is4() && addr.IsLoopback() || addr == netip.IPv6Loopback()
		}
		if !loopback {
			return config{}, errors.New("ORBITBEACON_URL requires HTTPS outside loopback")
		}
	}
	token := getenv("ORBITBEACON_TOKEN")
	if !tokenPattern.MatchString(token) {
		return config{}, errors.New("ORBITBEACON_TOKEN must be obt_ followed by 43 base64url characters")
	}
	u.Path = ""
	return config{baseURL: u.String(), token: token}, nil
}
