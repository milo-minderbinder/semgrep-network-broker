package it

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"github.com/returntocorp/semgrep-network-broker/pkg"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func mustGetRandomPrivateAddress() netip.Addr {
	addrBytes := netip.MustParseAddr("fdf0:59dc:33cf:9be8::0").AsSlice()
	rand.Read(addrBytes[8:])
	addr, ok := netip.AddrFromSlice(addrBytes)
	if !ok {
		panic("Failed to make random address")
	}
	return addr
}

func mustGetFreePort() int {
	addr, err := net.ResolveTCPAddr("tcp", "localhost:0")
	if err != nil {
		panic(err)
	}
	l, err := net.ListenTCP("tcp", addr)
	if err != nil {
		panic(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

type testClient struct {
	PeerAddress netip.Addr
	Client      *http.Client
}

func (tc *testClient) Request(method string, rawUrl string) (int, string, error) {
	url, err := url.Parse(rawUrl)
	if err != nil {
		return 0, "", err
	}

	req := &http.Request{
		Method: method,
		URL:    url,
	}
	resp, err := tc.Client.Do(req)
	if err != nil {
		return 0, "", err
	}

	defer resp.Body.Close()

	content := new(strings.Builder)
	io.Copy(content, resp.Body)

	return resp.StatusCode, content.String(), nil
}

func (tc *testClient) AssertStatusCode(t *testing.T, method string, rawUrl string, expectedStatusCode int) {
	statusCode, _, err := tc.Request(method, rawUrl)
	if err != nil {
		t.Errorf("error while making %v %v: %v", method, rawUrl, err)
	}

	if statusCode != expectedStatusCode {
		t.Errorf("%v %v returned HTTP %v, expected HTTP %v", method, rawUrl, statusCode, expectedStatusCode)
	}
}

func TestWireguardInboundProxy(t *testing.T) {
	gatewayWireguardPort := mustGetFreePort()
	gatewayWireguardAddress := mustGetRandomPrivateAddress()
	gatewayPrivateKey, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		panic(err)
	}
	gatewayPublicKey := gatewayPrivateKey.PublicKey()

	clientPrivateKey, _ := wgtypes.GeneratePrivateKey()
	clientPublicKey := clientPrivateKey.PublicKey()
	clientWireguardAddress := mustGetRandomPrivateAddress()

	// setup wireguard
	testWireguard := pkg.WireguardBase{
		LocalAddress: gatewayWireguardAddress.String(),
		PrivateKey:   hex.EncodeToString(gatewayPrivateKey[:]),
		Peers: []pkg.WireguardPeer{
			{
				PublicKey:  hex.EncodeToString(clientPublicKey[:]),
				AllowedIps: fmt.Sprintf("%v/128", clientWireguardAddress),
			},
		},
		ListenPort: gatewayWireguardPort,
	}
	testDev, testNet, err := pkg.SetupWireguard(&testWireguard, false)
	if err != nil {
		t.Errorf("failed to setup wireguard: %v", err)
	}

	if err := testDev.Up(); err != nil {
		t.Errorf("failed to bring up wireguard device: %v", err)
	}

	defer testDev.Down()

	tc := testClient{
		Client: &http.Client{
			Transport: &http.Transport{
				DialContext: testNet.DialContext,
			},
		},
		PeerAddress: clientWireguardAddress,
	}

	// set up internal service
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "Hello")
	}))

	defer server.Close()

	// start network broker
	inboundConfig := &pkg.InboundProxyConfig{
		Wireguard: pkg.WireguardBase{
			LocalAddress: clientWireguardAddress.String(),
			PrivateKey:   hex.EncodeToString(clientPrivateKey[:]),
			Peers: []pkg.WireguardPeer{
				{
					PublicKey:                   hex.EncodeToString(gatewayPublicKey[:]),
					AllowedIps:                  fmt.Sprintf("%v/128", gatewayWireguardAddress),
					Endpoint:                    fmt.Sprintf("127.0.0.1:%v", gatewayWireguardPort),
					PersistentKeepaliveInterval: 20,
				},
			},
		},
		Allowlist: []pkg.AllowlistItem{
			{
				URL:            server.URL + "/allowed-get",
				AllowedMethods: []string{"GET"},
			},
			{
				URL:            server.URL + "/allowed-post",
				AllowedMethods: []string{"POST"},
			},
		},
	}

	inboundTeardown, err := inboundConfig.Start(false)
	if err != nil {
		t.Error(err)
	}
	defer inboundTeardown()

	// it should proxy requests that match the allowlist
	tc.AssertStatusCode(t, "GET", fmt.Sprintf("http://[%v]/proxy/%v/allowed-get", clientWireguardAddress, server.URL), 200)
	tc.AssertStatusCode(t, "POST", fmt.Sprintf("http://[%v]/proxy/%v/allowed-post", clientWireguardAddress, server.URL), 200)

	// it should reject requests that don't match the allowlist
	tc.AssertStatusCode(t, "POST", fmt.Sprintf("http://[%v]/proxy/%v/allowed-get", clientWireguardAddress, server.URL), 403)
	tc.AssertStatusCode(t, "GET", fmt.Sprintf("http://[%v]/proxy/%v/allowed-post", clientWireguardAddress, server.URL), 403)
	tc.AssertStatusCode(t, "GET", fmt.Sprintf("http://[%v]/proxy/https://google.com", clientWireguardAddress), 403)
}
