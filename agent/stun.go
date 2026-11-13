package main

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"
)

// A minimal STUN client (RFC 5389 binding, RFC 5780 NAT behavior discovery).
// sbdbar asked sing-box to run STUN through its "direct" outbound; honk has
// no such API, so the agent runs it itself. Its UDP goes through honk's
// routing like any host traffic: a mainland STUN server matches geoip:cn and
// goes direct, so the result is this host's real public address.

const defaultSTUNServer = "stun.miwifi.com:3478"

const (
	stunMagic          = 0x2112A442
	stunBindingRequest = 0x0001
	stunBindingSuccess = 0x0101
	attrMappedAddress  = 0x0001
	attrChangeRequest  = 0x0003
	attrXorMapped      = 0x0020
	attrOtherAddress   = 0x802C
	attrChangedAddress = 0x0005
)

type stunAnswer struct {
	mapped  *net.UDPAddr
	other   *net.UDPAddr
	latency time.Duration
}

func stunRequest(changeIP, changePort bool) ([]byte, [12]byte) {
	var txid [12]byte
	_, _ = rand.Read(txid[:])
	var attrs []byte
	if changeIP || changePort {
		var flags uint32
		if changeIP {
			flags |= 0x4
		}
		if changePort {
			flags |= 0x2
		}
		attrs = binary.BigEndian.AppendUint16(attrs, attrChangeRequest)
		attrs = binary.BigEndian.AppendUint16(attrs, 4)
		attrs = binary.BigEndian.AppendUint32(attrs, flags)
	}
	msg := binary.BigEndian.AppendUint16(nil, stunBindingRequest)
	msg = binary.BigEndian.AppendUint16(msg, uint16(len(attrs)))
	msg = binary.BigEndian.AppendUint32(msg, stunMagic)
	msg = append(msg, txid[:]...)
	return append(msg, attrs...), txid
}

func parseSTUN(b []byte, txid [12]byte) (*stunAnswer, error) {
	if len(b) < 20 || binary.BigEndian.Uint16(b[0:2]) != stunBindingSuccess ||
		binary.BigEndian.Uint32(b[4:8]) != stunMagic || string(b[8:20]) != string(txid[:]) {
		return nil, errors.New("not a matching STUN binding response")
	}
	end := 20 + int(binary.BigEndian.Uint16(b[2:4]))
	if end > len(b) {
		return nil, errors.New("truncated STUN response")
	}
	ans := &stunAnswer{}
	for p := 20; p+4 <= end; {
		t := binary.BigEndian.Uint16(b[p:])
		l := int(binary.BigEndian.Uint16(b[p+2:]))
		v := b[p+4 : min(p+4+l, end)]
		switch t {
		case attrXorMapped:
			ans.mapped = decodeAddr(v, true, b[4:20])
		case attrMappedAddress:
			if ans.mapped == nil {
				ans.mapped = decodeAddr(v, false, nil)
			}
		case attrOtherAddress, attrChangedAddress:
			if ans.other == nil {
				ans.other = decodeAddr(v, false, nil)
			}
		}
		p += 4 + (l+3)&^3
	}
	if ans.mapped == nil {
		return nil, errors.New("STUN response has no mapped address")
	}
	return ans, nil
}

// decodeAddr reads a (XOR-)MAPPED-ADDRESS value; key is magic cookie + txid.
func decodeAddr(v []byte, xor bool, key []byte) *net.UDPAddr {
	if len(v) < 8 {
		return nil
	}
	port := binary.BigEndian.Uint16(v[2:4])
	var ip net.IP
	switch v[1] {
	case 0x01:
		ip = net.IP(append([]byte(nil), v[4:8]...))
	case 0x02:
		if len(v) < 20 {
			return nil
		}
		ip = net.IP(append([]byte(nil), v[4:20]...))
	default:
		return nil
	}
	if xor {
		port ^= uint16(stunMagic >> 16)
		for i := range ip {
			ip[i] ^= key[i]
		}
	}
	return &net.UDPAddr{IP: ip, Port: int(port)}
}

// stunExchange sends one request from conn to server and waits for the answer.
// A response may arrive from a different address (change requests).
func stunExchange(conn *net.UDPConn, server *net.UDPAddr, changeIP, changePort bool, timeout time.Duration) (*stunAnswer, error) {
	msg, txid := stunRequest(changeIP, changePort)
	buf := make([]byte, 1500)
	deadline := time.Now().Add(timeout)
	// Retransmit at 500ms intervals until the deadline.
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		start := time.Now()
		if _, err := conn.WriteToUDP(msg, server); err != nil {
			return nil, err
		}
		_ = conn.SetReadDeadline(minTime(deadline, start.Add(500*time.Millisecond)))
		for {
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					break
				}
				return nil, err
			}
			if ans, err := parseSTUN(buf[:n], txid); err == nil {
				ans.latency = time.Since(start)
				return ans, nil
			}
		}
	}
	return nil, fmt.Errorf("no STUN response from %s (i/o timeout)", server)
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

var natBehaviors = []string{"unknown", "endpoint-independent", "address-dependent", "address-and-port-dependent"}

// runSTUN emits sbdbar's stun lines: binding, nat-mapping, nat-filtering, done.
func runSTUN(out *lineWriter, server string) {
	if server == "" {
		server = defaultSTUNServer
	}
	base := map[string]any{"type": "stun", "outbound": "direct", "server": server}
	line := func(kv map[string]any) map[string]any {
		m := map[string]any{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range kv {
			m[k] = v
		}
		return m
	}
	fail := func(err error) {
		out.emit(line(map[string]any{"isFinal": true, "error": err.Error()}))
	}

	raddr, err := net.ResolveUDPAddr("udp4", server)
	if err != nil {
		fail(err)
		return
	}
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		fail(err)
		return
	}
	defer conn.Close()

	t1, err := stunExchange(conn, raddr, false, false, 3*time.Second)
	if err != nil {
		fail(err)
		return
	}
	res := map[string]any{
		"phase": "binding", "externalAddr": t1.mapped.String(), "latencyMs": t1.latency.Milliseconds(),
		"natMapping": "unknown", "natFiltering": "unknown", "natTypeSupported": t1.other != nil, "isFinal": false,
	}
	out.emit(line(res))
	if t1.other == nil {
		res["phase"], res["isFinal"] = "done", true
		out.emit(line(res))
		return
	}

	// Mapping (RFC 5780 4.3): same local socket to other IP, primary port; then other IP and port.
	mapping := 0
	t2, err := stunExchange(conn, &net.UDPAddr{IP: t1.other.IP, Port: raddr.Port}, false, false, 2*time.Second)
	if err == nil {
		if t2.mapped.String() == t1.mapped.String() {
			mapping = 1
		} else if t3, err := stunExchange(conn, t1.other, false, false, 2*time.Second); err == nil {
			if t3.mapped.String() == t2.mapped.String() {
				mapping = 2
			} else {
				mapping = 3
			}
		}
	}
	res["phase"], res["natMapping"] = "nat-mapping", natBehaviors[mapping]
	out.emit(line(res))

	// Filtering (RFC 5780 4.4): ask for replies from other IP+port, then other port only.
	filtering := 0
	if _, err := stunExchange(conn, raddr, true, true, 2*time.Second); err == nil {
		filtering = 1
	} else if _, err := stunExchange(conn, raddr, false, true, 2*time.Second); err == nil {
		filtering = 2
	} else {
		filtering = 3
	}
	res["phase"], res["natFiltering"] = "nat-filtering", natBehaviors[filtering]
	out.emit(line(res))
	res["phase"], res["isFinal"] = "done", true
	out.emit(line(res))
}
