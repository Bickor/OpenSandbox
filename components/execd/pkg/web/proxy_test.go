// Copyright 2026 The OpenSandbox Authors
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
// limitations under the License.

package web

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/execd/pkg/activity"
)

func TestProxyMiddlewareRecordsWebSocketHandshakeBeforeDisconnect(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()
		_, _, _ = conn.ReadMessage()
	}))
	defer backend.Close()

	backendURL, err := url.Parse(backend.URL)
	require.NoError(t, err)
	_, port, err := net.SplitHostPort(backendURL.Host)
	require.NoError(t, err)

	tracker := activity.NewTracker()
	router := gin.New()
	router.Use(ProxyMiddleware(tracker))
	proxyServer := httptest.NewServer(router)
	defer proxyServer.Close()

	wsURL := "ws" + strings.TrimPrefix(proxyServer.URL, "http") + "/proxy/" + port + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer conn.Close()

	require.EqualValues(t, 1, tracker.Snapshot().Revision)
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte("close")))
}

func TestProxyMiddlewareReturnsSidecarForbiddenForActiveVault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	receivedPath := make(chan string, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath <- r.URL.Path
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer backend.Close()

	backendURL, err := url.Parse(backend.URL)
	require.NoError(t, err)
	_, port, err := net.SplitHostPort(backendURL.Host)
	require.NoError(t, err)

	router := gin.New()
	router.Use(ProxyMiddleware(activity.NewTracker()))
	proxyServer := httptest.NewServer(router)
	defer proxyServer.Close()

	req, err := http.NewRequest(http.MethodGet, proxyServer.URL+"/proxy/"+port+"/credential-vault/_active", nil)
	require.NoError(t, err)
	resp, err := proxyServer.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.Equal(t, "/credential-vault/_active", <-receivedPath)
}
