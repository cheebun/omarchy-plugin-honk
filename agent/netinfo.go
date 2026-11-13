package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ipProviders maps the provider names shown in Settings to their JSON endpoints.
// The request goes through the system network, so with honk intercepting this host's traffic the
// answer is the proxy egress address that websites see.
var ipProviders = map[string]string{
	"ip.sb":    "https://api.ip.sb/geoip",
	"ipwho.is": "https://ipwho.is/",
	"ipapi.is": "https://api.ipapi.is/",
}

const defaultIPProvider = "ip.sb"

// ipInfo is the provider-independent shape returned by the ip command.
type ipInfo struct {
	Success     bool   `json:"success"`
	Provider    string `json:"provider"`
	IP          string `json:"ip"`
	Country     string `json:"country"`
	CountryCode string `json:"countryCode"`
	Region      string `json:"region"`
	City        string `json:"city"`
	ASN         string `json:"asn"`
	Org         string `json:"org"`
	Error       string `json:"error,omitempty"`
}

func lookupIP(ctx context.Context, client *http.Client, provider string) ipInfo {
	endpoint, ok := ipProviders[provider]
	if !ok {
		return ipInfo{Provider: provider, Error: fmt.Sprintf("unknown provider %q", provider)}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ipInfo{Provider: provider, Error: err.Error()}
	}
	req.Header.Set("User-Agent", "honk-agent")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return ipInfo{Provider: provider, Error: err.Error()}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ipInfo{Provider: provider, Error: err.Error()}
	}
	if resp.StatusCode != http.StatusOK {
		return ipInfo{Provider: provider, Error: fmt.Sprintf("%s returned HTTP %d", provider, resp.StatusCode)}
	}
	info, err := parseIPResponse(provider, body)
	if err != nil {
		return ipInfo{Provider: provider, Error: err.Error()}
	}
	return info
}

// parseIPResponse normalizes the three providers' JSON into ipInfo.
func parseIPResponse(provider string, body []byte) (ipInfo, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return ipInfo{}, fmt.Errorf("%s: invalid JSON: %w", provider, err)
	}
	str := func(m map[string]any, key string) string {
		switch v := m[key].(type) {
		case string:
			return v
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64)
		}
		return ""
	}
	sub := func(key string) map[string]any {
		m, _ := raw[key].(map[string]any)
		if m == nil {
			m = map[string]any{}
		}
		return m
	}

	info := ipInfo{Provider: provider, IP: str(raw, "ip")}
	switch provider {
	case "ip.sb":
		info.Country, info.CountryCode = str(raw, "country"), str(raw, "country_code")
		info.Region, info.City = str(raw, "region"), str(raw, "city")
		info.ASN, info.Org = str(raw, "asn"), str(raw, "organization")
	case "ipwho.is":
		if ok, present := raw["success"].(bool); present && !ok {
			return ipInfo{}, fmt.Errorf("ipwho.is: %s", str(raw, "message"))
		}
		conn := sub("connection")
		info.Country, info.CountryCode = str(raw, "country"), str(raw, "country_code")
		info.Region, info.City = str(raw, "region"), str(raw, "city")
		info.ASN, info.Org = str(conn, "asn"), str(conn, "org")
	case "ipapi.is":
		// The free tier answers with nested objects; tolerate the flat form too.
		loc, asn := sub("location"), sub("asn")
		info.Country, info.CountryCode = firstNonEmpty(str(loc, "country"), str(raw, "country")), str(loc, "country_code")
		info.Region, info.City = firstNonEmpty(str(loc, "state"), str(raw, "region")), firstNonEmpty(str(loc, "city"), str(raw, "city"))
		info.ASN, info.Org = str(asn, "asn"), firstNonEmpty(str(asn, "org"), str(sub("company"), "name"))
		if info.ASN == "" {
			// Flat form: "asn": "AS3258 xTom Japan Corporation"
			if s, ok := raw["asn"].(string); ok {
				num, org, _ := strings.Cut(s, " ")
				info.ASN, info.Org = strings.TrimPrefix(num, "AS"), firstNonEmpty(info.Org, org)
			}
		}
		info.Org = firstNonEmpty(info.Org, str(raw, "company"))
	}
	info.ASN = strings.TrimPrefix(info.ASN, "AS")
	if info.IP == "" {
		return ipInfo{}, errors.New(provider + ": no ip in response")
	}
	info.Success = true
	return info, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func runIP(provider string) ipInfo {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	return lookupIP(ctx, &http.Client{}, provider)
}
