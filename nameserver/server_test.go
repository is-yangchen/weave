package nameserver

import (
	"encoding/binary"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
	. "github.com/weaveworks/weave/common"
	wt "github.com/weaveworks/weave/testing"
)

const (
	testRDNSsuccess  = "1.2.2.10.in-addr.arpa."
	testRDNSfail     = "4.3.2.1.in-addr.arpa."
	testRDNSnonlocal = "8.8.8.8.in-addr.arpa."
	testUDPBufSize   = 16384
)

func setupForTest(t *testing.T) {
	// fail early if we cannot find a default multicast interface
	multicast, err := LinkLocalMulticastListener(nil)
	if err != nil {
		t.Fatalf("Unable to create multicast listener: %s. No default multicast interface?", err)
	}
	multicast.Close()
}

func TestUDPDNSServer(t *testing.T) {
	setupForTest(t)

	const (
		successTestName = "test1.weave.local."
		failTestName    = "fail.weave.local."
		nonLocalName    = "weave.works."
		testAddr1       = "10.2.2.1"
		containerID     = "somecontainer"
	)
	testCIDR1 := testAddr1 + "/24"

	InitDefaultLogging(testing.Verbose())
	Info.Println("TestUDPDNSServer starting")

	zone, err := NewZoneDb(ZoneConfig{})
	wt.AssertNoErr(t, err)
	err = zone.Start()
	wt.AssertNoErr(t, err)
	defer zone.Stop()

	ip, _, _ := net.ParseCIDR(testCIDR1)
	zone.AddRecord(containerID, successTestName, ip)

	fallbackHandler := func(w dns.ResponseWriter, req *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(req)
		if len(req.Question) == 1 {
			q := req.Question[0]
			if q.Name == nonLocalName && q.Qtype == dns.TypeMX {
				m.Answer = make([]dns.RR, 1)
				m.Answer[0] = &dns.MX{Hdr: dns.RR_Header{Name: m.Question[0].Name, Rrtype: dns.TypeMX, Class: dns.ClassINET, Ttl: 0}, Mx: "mail." + nonLocalName}
			} else if q.Name == nonLocalName && q.Qtype == dns.TypeANY {
				m.Answer = make([]dns.RR, 512/len("mailn."+nonLocalName)+1)
				for i := range m.Answer {
					m.Answer[i] = &dns.MX{Hdr: dns.RR_Header{Name: m.Question[0].Name, Rrtype: dns.TypeMX, Class: dns.ClassINET, Ttl: 0}, Mx: fmt.Sprintf("mail%d.%s", i, nonLocalName)}
				}
			} else if q.Name == testRDNSnonlocal && q.Qtype == dns.TypePTR {
				m.Answer = make([]dns.RR, 1)
				m.Answer[0] = &dns.PTR{Hdr: dns.RR_Header{Name: m.Question[0].Name, Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: 0}, Ptr: "ns1.google.com."}
			} else if q.Name == testRDNSfail && q.Qtype == dns.TypePTR {
				m.Rcode = dns.RcodeNameError
			}
		}
		w.WriteMsg(m)
	}

	// Run another DNS server for fallback
	fallback, err := newMockedFallback(fallbackHandler, nil)
	wt.AssertNoErr(t, err)
	fallback.Start()
	defer fallback.Stop()

	srv, err := NewDNSServer(DNSServerConfig{
		Zone:              zone,
		UpstreamCfg:       fallback.CliConfig,
		CacheDisabled:     true,
		ListenReadTimeout: testSocketTimeout,
	})
	wt.AssertNoErr(t, err)
	go srv.Start()
	defer srv.Stop()
	time.Sleep(100 * time.Millisecond) // Allow sever goroutine to start

	var r *dns.Msg

	testPort, err := srv.GetPort()
	wt.AssertNoErr(t, err)
	wt.AssertNotEqualInt(t, testPort, 0, "invalid listen port")

	_, r = assertExchange(t, successTestName, dns.TypeA, testPort, 1, 1, 0)
	wt.AssertType(t, r.Answer[0], (*dns.A)(nil), "DNS record")
	wt.AssertEqualString(t, r.Answer[0].(*dns.A).A.String(), testAddr1, "IP address")

	assertExchange(t, failTestName, dns.TypeA, testPort, 0, 0, dns.RcodeNameError)

	_, r = assertExchange(t, testRDNSsuccess, dns.TypePTR, testPort, 1, 1, 0)
	wt.AssertType(t, r.Answer[0], (*dns.PTR)(nil), "DNS record")
	wt.AssertEqualString(t, r.Answer[0].(*dns.PTR).Ptr, successTestName, "IP address")

	assertExchange(t, testRDNSfail, dns.TypePTR, testPort, 0, 0, dns.RcodeNameError)

	// This should fail because we don't handle MX records
	assertExchange(t, successTestName, dns.TypeMX, testPort, 0, 0, dns.RcodeNameError)

	// This non-local query for an MX record should succeed by being
	// passed on to the fallback server
	assertExchange(t, nonLocalName, dns.TypeMX, testPort, 1, -1, 0)

	// Now ask a query that we expect to return a lot of data.
	assertExchange(t, nonLocalName, dns.TypeANY, testPort, 5, -1, 0)

	assertExchange(t, testRDNSnonlocal, dns.TypePTR, testPort, 1, -1, 0)

	// Not testing MDNS functionality of server here (yet), since it
	// needs two servers, each listening on its own address
}

func TestTCPDNSServer(t *testing.T) {
	setupForTest(t)

	const (
		numAnswers   = 512
		nonLocalName = "weave.works."
	)

	InitDefaultLogging(testing.Verbose())
	Info.Println("TestTCPDNSServer starting")

	zone, err := NewZoneDb(ZoneConfig{})
	wt.AssertNoErr(t, err)
	err = zone.Start()
	wt.AssertNoErr(t, err)
	defer zone.Stop()

	// generate a list of `numAnswers` IP addresses
	var addrs []ZoneRecord
	bs := make([]byte, 4)
	for i := 0; i < numAnswers; i++ {
		binary.LittleEndian.PutUint32(bs, uint32(i))
		ip := net.IPv4(bs[0], bs[1], bs[2], bs[3])
		addrs = append(addrs, ZoneRecord(Record{"", ip, 0, 0, 0}))
	}

	// handler for the fallback server: it will just return a very long response
	fallbackUDPHandler := func(w dns.ResponseWriter, req *dns.Msg) {
		maxLen := getMaxReplyLen(req, protUDP)

		t.Logf("Fallback UDP server got asked: returning %d answers", numAnswers)
		q := req.Question[0]
		m := makeAddressReply(req, &q, addrs, DefaultLocalTTL)
		mLen := m.Len()
		m.SetEdns0(uint16(maxLen), false)

		if mLen > maxLen {
			t.Logf("... truncated response (%d > %d)", mLen, maxLen)
			m.Truncated = true
		}
		w.WriteMsg(m)
	}
	fallbackTCPHandler := func(w dns.ResponseWriter, req *dns.Msg) {
		t.Logf("Fallback TCP server got asked: returning %d answers", numAnswers)
		q := req.Question[0]
		m := makeAddressReply(req, &q, addrs, DefaultLocalTTL)
		w.WriteMsg(m)
	}

	t.Logf("Running a DNS fallback server with UDP")
	fallback, err := newMockedFallback(fallbackUDPHandler, fallbackTCPHandler)
	wt.AssertNoErr(t, err)
	fallback.Start()
	defer fallback.Stop()

	t.Logf("Creating a WeaveDNS server instance, falling back to 127.0.0.1:%d", fallback.Port)
	srv, err := NewDNSServer(DNSServerConfig{
		Zone:              zone,
		UpstreamCfg:       fallback.CliConfig,
		CacheDisabled:     true,
		ListenReadTimeout: testSocketTimeout,
	})
	wt.AssertNoErr(t, err)
	go srv.Start()
	defer srv.Stop()
	time.Sleep(100 * time.Millisecond) // Allow sever goroutine to start

	testPort, err := srv.GetPort()
	wt.AssertNoErr(t, err)
	wt.AssertNotEqualInt(t, testPort, 0, "listen port")
	dnsAddr := fmt.Sprintf("127.0.0.1:%d", testPort)

	t.Logf("Creating a UDP and a TCP client")
	uc := new(dns.Client)
	uc.UDPSize = minUDPSize
	tc := new(dns.Client)
	tc.Net = "tcp"

	t.Logf("Creating DNS query message")
	m := new(dns.Msg)
	m.RecursionDesired = true
	m.SetQuestion(nonLocalName, dns.TypeA)

	t.Logf("Checking the fallback server at %s returns a truncated response with UDP", fallback.Addr)
	r, _, err := uc.Exchange(m, fallback.Addr)
	t.Logf("Got response from fallback server (UDP) with %d answers", len(r.Answer))
	t.Logf("Response:\n%+v\n", r)
	wt.AssertNoErr(t, err)
	wt.AssertTrue(t, r.MsgHdr.Truncated, "DNS truncated reponse flag")
	wt.AssertNotEqualInt(t, len(r.Answer), numAnswers, "number of answers (UDP)")

	t.Logf("Checking the WeaveDNS server at %s returns a truncated reponse with UDP", dnsAddr)
	r, _, err = uc.Exchange(m, dnsAddr)
	t.Logf("UDP Response:\n%+v\n", r)
	wt.AssertNoErr(t, err)
	wt.AssertNotNil(t, r, "response")
	t.Logf("%d answers", len(r.Answer))
	wt.AssertTrue(t, r.MsgHdr.Truncated, "DNS truncated reponse flag")
	wt.AssertNotEqualInt(t, len(r.Answer), numAnswers, "number of answers (UDP)")

	t.Logf("Checking the WeaveDNS server at %s does not return a truncated reponse with TCP", dnsAddr)
	r, _, err = tc.Exchange(m, dnsAddr)
	t.Logf("TCP Response:\n%+v\n", r)
	wt.AssertNoErr(t, err)
	wt.AssertNotNil(t, r, "response")
	t.Logf("%d answers", len(r.Answer))
	wt.AssertFalse(t, r.MsgHdr.Truncated, "DNS truncated response flag")
	wt.AssertEqualInt(t, len(r.Answer), numAnswers, "number of answers (TCP)")

	t.Logf("Checking the WeaveDNS server at %s does not return a truncated reponse with UDP with a bigger buffer", dnsAddr)
	m.SetEdns0(testUDPBufSize, false)
	r, _, err = uc.Exchange(m, dnsAddr)
	t.Logf("UDP-large Response:\n%+v\n", r)
	wt.AssertNoErr(t, err)
	wt.AssertNotNil(t, r, "response")
	t.Logf("%d answers", len(r.Answer))
	wt.AssertNoErr(t, err)
	wt.AssertFalse(t, r.MsgHdr.Truncated, "DNS truncated response flag")
	wt.AssertEqualInt(t, len(r.Answer), numAnswers, "number of answers (UDP-long)")
}
