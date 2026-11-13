package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Fixtures follow docs/reference/openapi/native_api_openapi.yaml and values
// observed from honk v0.1.0-beta.5 (UInt64 fields are decimal strings).

func TestU64DecodesWireStrings(t *testing.T) {
	var v struct {
		A u64 `json:"a"`
		B u64 `json:"b"`
		C u64 `json:"c"`
	}
	if err := json.Unmarshal([]byte(`{"a":"2048","b":null,"c":7}`), &v); err != nil {
		t.Fatal(err)
	}
	if !v.A.Valid || v.A.V != 2048 || v.B.Valid || !v.C.Valid || v.C.V != 7 {
		t.Errorf("got %+v", v)
	}
}

func TestNewClientRefusesCleartextSecretToRemote(t *testing.T) {
	if _, err := newClient("http://192.0.2.1:9527", "s"); !errors.Is(err, errConfig) {
		t.Errorf("remote cleartext with secret: err = %v", err)
	}
	if _, err := newClient("http://192.0.2.1:9527", ""); err != nil {
		t.Errorf("remote without secret: %v", err)
	}
	c, err := newClient("127.0.0.1:9527/", "s")
	if err != nil || c.base != "http://127.0.0.1:9527/api/v1" {
		t.Errorf("loopback shorthand: base=%q err=%v", c.base, err)
	}
	if _, err := newClient("ftp://127.0.0.1", ""); !errors.Is(err, errConfig) {
		t.Errorf("ftp: err = %v", err)
	}
}

func TestStatusCodeMapping(t *testing.T) {
	cases := map[error]int{
		&apiError{Status: 401}:                  401,
		&apiError{Status: 403}:                  401,
		&apiError{Status: 500}:                  500,
		errors.New("connection refused"):        0,
		errors.Join(errConfig, errors.New("x")): 500,
	}
	for err, want := range cases {
		if got := statusCode(err); got != want {
			t.Errorf("statusCode(%v) = %d, want %d", err, got, want)
		}
	}
}

func f(v float64) *float64 { return &v }

func TestNodeDelay(t *testing.T) {
	healthy := nodeDoc{Health: []healthObs{
		{Transport: "udp", Purpose: "data", State: "healthy", LatencyMs: f(5)},
		{Transport: "tcp", Purpose: "data", State: "healthy", LatencyMs: f(80), MovingAvgMs: f(74.6)},
	}}
	if d := nodeDelay(healthy); d != 75 {
		t.Errorf("healthy = %d, want 75 (moving average, TCP only)", d)
	}
	down := nodeDoc{Health: []healthObs{{Transport: "tcp", Purpose: "data", State: "unavailable"}}}
	if d := nodeDelay(down); d != -1 {
		t.Errorf("unavailable = %d, want -1", d)
	}
	if d := nodeDelay(nodeDoc{}); d != 0 {
		t.Errorf("unknown = %d, want 0", d)
	}
}

func TestBuildGroupsLine(t *testing.T) {
	vless := "vless"
	nodes := map[string]nodeDoc{
		"n1": {ID: "n1", Name: "JPN.A", Protocol: &vless, Health: []healthObs{{Transport: "tcp", Purpose: "data", State: "healthy", LatencyMs: f(90)}}},
		"n2": {ID: "n2", Name: "HKG.B", Protocol: &vless},
		"d":  {ID: "d", Name: "direct"},
	}
	proxies := groupDetail{ID: "g1", Name: "Proxies", Members: []groupMember{{"n1", "JPN.A", "node"}, {"n2", "HKG.B", "node"}}}
	proxies.Policy.Kind = "selector"
	proxies.Capabilities.CanSelect = true
	proxies.Runtime.Selection.TCP = &groupSelection{MemberID: "n1", ResolvedLeaf: "n1", Source: "runtime"}
	final := groupDetail{ID: "g2", Name: "Final", Members: []groupMember{{"d", "direct", "node"}, {"g1", "Proxies", "group"}}}
	final.Policy.Kind = "urltest"
	final.Capabilities.CanOverride = true
	final.Runtime.Selection.TCP = &groupSelection{MemberID: "g1", ResolvedLeaf: "n1", Source: "override"}

	line := buildGroupsLine([]groupDetail{proxies, final}, nodes)
	groups := line["groups"].([]groupJSON)
	if line["activeNode"] != "JPN.A" || len(groups) != 2 {
		t.Fatalf("line = %+v", line)
	}
	if g := groups[0]; g.Selected != "JPN.A" || !g.Selectable || g.Pinned || g.Items[0].Type != "vless" || g.Items[0].Delay != 90 {
		t.Errorf("Proxies = %+v", g)
	}
	g := groups[1]
	if g.Selected != "Proxies" || !g.Selectable || !g.Pinned {
		t.Errorf("Final = %+v", g)
	}
	if g.Items[0].Type != "direct" || g.Items[1].Type != "selector" || g.Items[1].Delay != 90 {
		t.Errorf("Final items = %+v (nested group shows its leaf's delay)", g.Items)
	}
}

func TestConnTrackerRatesFromDeltas(t *testing.T) {
	dst, dom, out := "142.250.0.1:443", "www.google.com", "Final"
	conn := func(up, down string) connDoc {
		var c connDoc
		_ = json.Unmarshal([]byte(`{"id":"c1","state":"active","upload_bytes":"`+up+`","download_bytes":"`+down+`",
			"upload_bytes_per_second":null,"download_bytes_per_second":null,"chain":["g2","g1","n1"]}`), &c)
		c.Dst, c.Domain, c.Outbound = &dst, &dom, &out
		return c
	}
	names := map[string]string{"g2": "Final", "g1": "Proxies", "n1": "JPN.A"}
	tr := newConnTracker()
	t0 := time.Unix(1000, 0)
	first := tr.update(connListDoc{TCP: []connDoc{conn("100", "1000")}}, t0, names)
	if first[0].UpRate != "↑ 0 B/s" || first[0].Network != "TCP" || first[0].Host != "www.google.com" {
		t.Errorf("first = %+v", first[0])
	}
	second := tr.update(connListDoc{TCP: []connDoc{conn("2148", "1000")}}, t0.Add(2*time.Second), names)
	c := second[0]
	if c.UpRate != "↑ 1.0 KB/s" || c.DownRate != "↓ 0 B/s" || c.CreatedAt != t0.UnixMilli() {
		t.Errorf("second = %+v", c)
	}
	if c.Chain != "Final → Proxies → JPN.A" || c.Status != "Active" {
		t.Errorf("chain/status = %q %q", c.Chain, c.Status)
	}
	if got := tr.update(connListDoc{}, t0.Add(3*time.Second), names); len(got) != 0 || len(tr.seen) != 0 {
		t.Errorf("closed connections must be dropped: %+v", got)
	}
}

func TestReadSSE(t *testing.T) {
	body := ": heartbeat\n\nevent: stream.ready\nid: abc\ndata: {\"instance_id\":\"i\"}\n\n" +
		"event: log\ndata: {\"ts\":\"2026-11-04T02:00:00Z\",\"level\":\"info\",\n" +
		"data: \"target\":\"honk_core\",\"message\":\"m\",\"fields\":null}\n\n"
	ch := make(chan sseEvent, 4)
	err := readSSE(strings.NewReader(body), ch)
	close(ch)
	if err == nil {
		t.Error("end of stream must be reported")
	}
	var got []sseEvent
	for e := range ch {
		got = append(got, e)
	}
	if len(got) != 2 || got[0].Event != "stream.ready" || got[0].ID != "abc" || got[1].Event != "log" {
		t.Fatalf("events = %+v", got)
	}
	var rec logRecord
	if err := json.Unmarshal([]byte(got[1].Data), &rec); err != nil || rec.Message != "m" {
		t.Errorf("multi-line data = %q (%v)", got[1].Data, err)
	}
}

func TestLogEntry(t *testing.T) {
	target := "honk_core::control"
	e := logEntry(logRecord{TS: "2026-11-04T02:00:00Z", Level: "warn", Target: &target, Message: "reload rejected",
		Fields: map[string]any{"stage": "x", "a": 1}}, 7)
	if e.Level != "warn" || e.Seq != "7" || e.Message != "reload rejected a=1 stage=x" {
		t.Errorf("entry = %+v", e)
	}
	if !strings.HasSuffix(e.Raw, " honk_core::control: reload rejected a=1 stage=x") {
		t.Errorf("raw = %q", e.Raw)
	}
	if lvl, ok := parseLogLevel("WARNING"); !ok || lvl != "warn" {
		t.Errorf("parseLogLevel(WARNING) = %q %v", lvl, ok)
	}
	if _, ok := parseLogLevel("panic"); ok {
		t.Error("honk has no panic level")
	}
}

func TestParseSTUNXorMapped(t *testing.T) {
	msg, txid := stunRequest(false, false)
	if len(msg) != 20 || binary.BigEndian.Uint16(msg) != stunBindingRequest {
		t.Fatalf("request = %x", msg)
	}
	// Build a success response with XOR-MAPPED-ADDRESS 198.51.100.9:40000 and OTHER-ADDRESS.
	resp := binary.BigEndian.AppendUint16(nil, stunBindingSuccess)
	resp = binary.BigEndian.AppendUint16(resp, 24)
	resp = binary.BigEndian.AppendUint32(resp, stunMagic)
	resp = append(resp, txid[:]...)
	ip := net.ParseIP("198.51.100.9").To4()
	resp = binary.BigEndian.AppendUint16(resp, attrXorMapped)
	resp = binary.BigEndian.AppendUint16(resp, 8)
	resp = append(resp, 0, 1)
	resp = binary.BigEndian.AppendUint16(resp, 40000^uint16(stunMagic>>16))
	for i := range ip {
		resp = append(resp, ip[i]^resp[4+i])
	}
	resp = binary.BigEndian.AppendUint16(resp, attrOtherAddress)
	resp = binary.BigEndian.AppendUint16(resp, 8)
	resp = append(resp, 0, 1, 0x0d, 0x96, 203, 0, 113, 5)
	ans, err := parseSTUN(resp, txid)
	if err != nil {
		t.Fatal(err)
	}
	if ans.mapped.String() != "198.51.100.9:40000" || ans.other.String() != "203.0.113.5:3478" {
		t.Errorf("mapped=%v other=%v", ans.mapped, ans.other)
	}
	var wrong [12]byte
	if _, err := parseSTUN(resp, wrong); err == nil {
		t.Error("mismatched transaction id must be rejected")
	}
}

// TestSelectAgainstFakeAPI drives select through an httptest server shaped
// like the native API and checks the request honk receives.
func TestSelectAgainstFakeAPI(t *testing.T) {
	var gotBody map[string]any
	var gotAuth, gotUnpin string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v1/groups":
			_, _ = w.Write([]byte(`[{"id":"g1","name":"Proxies"}]`))
		case r.Method == "GET" && r.URL.Path == "/api/v1/groups/g1":
			_, _ = w.Write([]byte(`{"id":"g1","name":"Proxies","policy":{"kind":"selector"},
				"members":[{"id":"n1","name":"JPN.A","kind":"node"}],
				"runtime":{"selection":{"tcp":null}},"capabilities":{"can_select":true,"can_override":false}}`))
		case r.Method == "PUT" && r.URL.Path == "/api/v1/groups/g1/selection":
			gotAuth = r.Header.Get("Authorization")
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == "DELETE" && r.URL.Path == "/api/v1/groups/g1/selection":
			gotUnpin = r.URL.Query().Get("network")
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":{"code":"resource_not_found","message":"nope"},"request_id":null}`))
		}
	}))
	defer srv.Close()
	c, err := newClient(srv.URL, "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if res := runAction(c, "select", []string{"Proxies", "JPN.A"}); res["success"] != true {
		t.Fatalf("select = %+v", res)
	}
	if gotAuth != "Bearer s3cret" || gotBody["member_id"] != "n1" || gotBody["network"] != "both" {
		t.Errorf("auth=%q body=%+v", gotAuth, gotBody)
	}
	if res := runAction(c, "unpin", []string{"Proxies"}); res["success"] != true || gotUnpin != "both" {
		t.Errorf("unpin = %+v, network=%q", res, gotUnpin)
	}
	if res := runAction(c, "set-mode", []string{"global"}); res["success"] != false {
		t.Errorf("set-mode without a /config endpoint must fail: %+v", res)
	}
	res := runAction(c, "select", []string{"Proxies", "nope"})
	if res["success"] != false || !strings.Contains(res["error"].(string), "not a member") {
		t.Errorf("unknown member = %+v", res)
	}
	if err := c.do(context.Background(), "GET", "/missing", nil, nil); statusCode(err) != 404 || !strings.Contains(errorMessage(err), "resource_not_found") {
		t.Errorf("api error = %v", err)
	}
}

// Shaped like config.d/proxy.dae: a leading (must) rule, then ordinary rules.
const routingSample = `node {
    'JPN.A': 'vless://x@192.0.2.1:443?security=reality#a'
}

routing {
    # honk sniffs and intercepts port 53 itself.
    dip(10.0.0.0/8, 192.168.0.0/16) && !dport(53) -> direct(must)
    domain(suffix: 'claude.ai') -> AI
    domain(regex: '(^|\.)facebook\.') -> Proxies # '{' in a comment
    fallback: Final
}

dns {
    routing {
        request {
            fallback: remote
        }
    }
}
`

func TestApplyModeGlobalDirectRule(t *testing.T) {
	g, err := applyMode(routingSample, "Global", "Proxies")
	if err != nil {
		t.Fatal(err)
	}
	want := "-> direct(must)\n    l4proto(tcp, udp) -> Proxies # doona: outbound mode\n    domain(suffix: 'claude.ai')"
	if !strings.Contains(g, want) {
		t.Fatalf("global insert misplaced:\n%s", g)
	}
	if m, tgt := readMode(g); m != "Global" || tgt != "Proxies" {
		t.Errorf("readMode(global) = %s %s", m, tgt)
	}
	d, err := applyMode(g, "Direct", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(d, modeMarker) != 1 || !strings.Contains(d, "l4proto(tcp, udp) -> direct "+modeMarker) {
		t.Errorf("direct must replace the global line:\n%s", d)
	}
	if m, _ := readMode(d); m != "Direct" {
		t.Errorf("readMode(direct) = %s", m)
	}
	r, err := applyMode(d, "Rule", "")
	if err != nil {
		t.Fatal(err)
	}
	if r != routingSample {
		t.Errorf("rule must restore the original text exactly:\n%s", r)
	}
	if m, _ := readMode(routingSample); m != "Rule" {
		t.Errorf("readMode(sample) = %s", m)
	}
}

func TestApplyModeOnlyTouchesTopLevelRouting(t *testing.T) {
	g, _ := applyMode(routingSample, "Direct", "")
	dnsPart := g[strings.Index(g, "dns {"):]
	if strings.Contains(dnsPart, modeMarker) {
		t.Error("the dns routing block must not be edited")
	}
	if _, err := applyMode("global {\n}\n", "Direct", ""); err == nil {
		t.Error("no routing section must be an error")
	}
}

func TestApplyModeRefusesInterleavedMust(t *testing.T) {
	src := "routing {\n    domain(suffix: x.com) -> Proxies\n    dip(10.0.0.0/8) -> direct(must)\n    fallback: direct\n}\n"
	if _, err := applyMode(src, "Global", "Proxies"); err == nil {
		t.Error("a (must) rule after an ordinary rule must be refused")
	}
	if _, err := applyMode(src, "Rule", ""); err != nil {
		t.Errorf("rule mode only removes the marker: %v", err)
	}
}

func TestApplyModeMultiLineMust(t *testing.T) {
	src := "routing {\n    sip(\n        10.10.10.24/32,\n        10.10.10.25/32\n    ) && !dport(53) -> direct(must)\n    domain(x) -> P\n}\n"
	g, err := applyMode(src, "Direct", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(g, "-> direct(must)\n    l4proto(tcp, udp) -> direct "+modeMarker+"\n    domain(x)") {
		t.Errorf("insert after a multi-line must rule:\n%s", g)
	}
}

// A line written by doona itself must read back and be removable.
func TestReadsDoonaLine(t *testing.T) {
	src := "routing {\n    l4proto(tcp, udp) -> AI # doona: outbound mode\n    fallback: Final\n}\n"
	if m, tgt := readMode(src); m != "Global" || tgt != "AI" {
		t.Errorf("readMode = %s %s", m, tgt)
	}
	r, _ := applyMode(src, "Rule", "")
	if strings.Contains(r, "doona") {
		t.Errorf("rule must remove doona's line: %s", r)
	}
}
