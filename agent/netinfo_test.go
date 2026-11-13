package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Sample bodies follow the real providers' field layout, with a documentation IP.
var ipSamples = map[string]string{
	"ip.sb": `{"organization":"xTom Japan Corporation","asn":3258,"ip":"203.0.113.7","country":"Japan",
		"country_code":"JP","region":"Tokyo","city":"Tokyo","asn_organization":"xTom Japan Corporation"}`,
	"ipwho.is": `{"ip":"203.0.113.7","success":true,"country":"Japan","country_code":"JP","region":"Tokyo","city":"Tokyo",
		"connection":{"asn":3258,"org":"xTom Japan Corporation","isp":"xTom Japan Corporation"}}`,
	"ipapi.is": `{"ip":"203.0.113.7","is_bogon":false,"company":"xTom Japan Corporation",
		"asn":"AS3258 xTom Japan Corporation","city":"Tokyo","region":"Tokyo","country":"Japan"}`,
}

func TestParseIPResponse(t *testing.T) {
	for provider, body := range ipSamples {
		got, err := parseIPResponse(provider, []byte(body))
		if err != nil {
			t.Fatalf("%s: %v", provider, err)
		}
		if !got.Success || got.IP != "203.0.113.7" || got.Country != "Japan" || got.City != "Tokyo" ||
			got.ASN != "3258" || got.Org != "xTom Japan Corporation" {
			t.Errorf("%s = %+v", provider, got)
		}
	}
	// ipapi.is nested form
	nested := `{"ip":"203.0.113.7","location":{"country":"Japan","country_code":"JP","state":"Tokyo","city":"Tokyo"},
		"asn":{"asn":3258,"org":"xTom Japan Corporation"}}`
	if got, err := parseIPResponse("ipapi.is", []byte(nested)); err != nil || got.ASN != "3258" || got.CountryCode != "JP" || got.Org != "xTom Japan Corporation" {
		t.Errorf("ipapi.is nested = %+v, %v", got, err)
	}
	if _, err := parseIPResponse("ipwho.is", []byte(`{"success":false,"message":"Reserved range"}`)); err == nil {
		t.Error("ipwho.is failure accepted")
	}
	if _, err := parseIPResponse("ip.sb", []byte(`{"country":"Japan"}`)); err == nil {
		t.Error("missing ip accepted")
	}
}

func TestLookupIPOverHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "honk-agent" {
			t.Errorf("User-Agent = %q", r.Header.Get("User-Agent"))
		}
		w.Write([]byte(ipSamples["ip.sb"]))
	}))
	defer srv.Close()
	old := ipProviders["ip.sb"]
	ipProviders["ip.sb"] = srv.URL
	defer func() { ipProviders["ip.sb"] = old }()

	if got := lookupIP(context.Background(), srv.Client(), "ip.sb"); !got.Success || got.IP != "203.0.113.7" {
		t.Errorf("lookupIP = %+v", got)
	}
	if got := lookupIP(context.Background(), srv.Client(), "nope"); got.Success || got.Error == "" {
		t.Errorf("unknown provider = %+v", got)
	}
}

func TestSetPref(t *testing.T) {
	cfg := Config{}
	for _, c := range []struct {
		key, value string
		ok         bool
	}{
		{"groupsView", "list", true}, {"groupsView", "grid", false},
		{"ipProvider", "ipwho.is", true}, {"ipProvider", "ipify", false},
		{"stunServer", "stun.douyucdn.cn:18000", true}, {"stunServer", "", false}, {"stunServer", "nohost", false},
		{"color", "red", false},
	} {
		if err := setPref(&cfg, c.key, c.value); (err == nil) != c.ok {
			t.Errorf("setPref(%s, %q) err = %v", c.key, c.value, err)
		}
	}
	if cfg.GroupsView != "list" || cfg.IPProvider != "ipwho.is" || cfg.STUNServer != "stun.douyucdn.cn:18000" {
		t.Errorf("cfg = %+v", cfg)
	}
}
