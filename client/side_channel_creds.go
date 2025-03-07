// Copyright (c) 2020 StackRox Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License

package client

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"

	"google.golang.org/grpc/credentials"
)

// sideChannelCreds implements gRPC transport credentials that do not modify the connection passed to `ClientHandshake`,
// but instead takes the `AuthInfo` from a connection established via a side channel.
type sideChannelCreds struct {
	credentials.TransportCredentials
	endpoint string

	authInfo      credentials.AuthInfo
	authInfoMutex sync.Mutex
}

func newCredsFromSideChannel(endpoint string, creds credentials.TransportCredentials) credentials.TransportCredentials {
	return &sideChannelCreds{
		TransportCredentials: creds,
		endpoint:             endpoint,
	}
}

func (c *sideChannelCreds) ClientHandshake(ctx context.Context, authority string, rawConn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	c.authInfoMutex.Lock()
	defer c.authInfoMutex.Unlock()

	if c.authInfo != nil {
		return rawConn, c.authInfo, nil
	}

	// check if c.endpoint is reached via proxy
	destReq, err := http.NewRequest("GET", "http://"+c.endpoint, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to determine proxy URL for %s: %w", c.endpoint, err)
	}
	proxyURL, err := http.ProxyFromEnvironment(destReq)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to determine proxy URL for %s: %w", c.endpoint, err)
	}

	var sideChannelConn net.Conn
	if proxyURL != nil {
		// net dial via HTTP CONNECT tunnel if using proxy
		sideChannelConn, err = dialViaCONNECT(ctx, c.endpoint, proxyURL)
	} else {
		sideChannelConn, err = new(net.Dialer).DialContext(ctx, "tcp", c.endpoint)
	}

	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = sideChannelConn.Close() }()

	_, authInfo, err := c.TransportCredentials.ClientHandshake(ctx, authority, sideChannelConn)
	if err != nil {
		return nil, nil, err
	}

	c.authInfo = authInfo
	return rawConn, authInfo, nil
}

// dialViaCONNECT tunnels a tcp connection to addr through proxy using HTTP CONNECT
func dialViaCONNECT(ctx context.Context, addr string, proxy *url.URL) (net.Conn, error) {
	proxyAddr := proxy.Host
	if proxy.Port() == "" {
		proxyAddr = net.JoinHostPort(proxyAddr, "80")
	}
	conn, err := new(net.Dialer).DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to dial proxy %s: %w", proxyAddr, err)
	}
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", addr, proxy.Hostname())
	rr := bufio.NewReader(conn)
	res, err := http.ReadResponse(rr, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to read response from HTTP CONNECT to %s via proxy %s: %w", addr, proxyAddr, err)
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to dial %s via %s. response status: %v", addr, proxyAddr, res.Status)
	}
	if rr.Buffered() > 0 {
		return nil, fmt.Errorf("CONNECT response from %s resulted in %d bytes of unexpected data", proxyAddr, rr.Buffered())
	}
	return conn, nil
}
