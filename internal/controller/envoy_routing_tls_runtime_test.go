//go:build envoy_integration

// Copyright 2026 Firebolt Analytics
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/routing"
	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/http2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

type routingTLSCA struct {
	certificate *x509.Certificate
	key         *ecdsa.PrivateKey
	pem         []byte
}

func newRoutingTLSCA(t *testing.T) routingTLSCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "routing test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return routingTLSCA{certificate: parsed, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func (ca routingTLSCA) leaf(t *testing.T, name string, serial int64, extraDNS ...string) (signed tls.Certificate, chainPEM, privateKeyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name}, DNSNames: append([]string{name}, extraDNS...),
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.certificate, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return certificate, certPEM, keyPEM
}

func routingTLSTransport(t *testing.T, snippet string) any {
	t.Helper()
	var transport map[string]any
	if err := yaml.Unmarshal([]byte(snippet), &transport); err != nil {
		t.Fatal(err)
	}
	if transport["transport_socket"] == nil {
		t.Fatal("production TLS transport was not rendered")
	}
	return transport["transport_socket"]
}

// This test uses the production TLS transports, mandatory external-processing
// filter and generation probe listener. The admission double isolates transport
// compatibility; request accounting itself has production-agent runtime tests.
func TestEnvoyRoutingTLSProbeAndQueries(t *testing.T) {
	ca := newRoutingTLSCA(t)
	generationDNS := routing.GenerationServiceName("runtime-engine-uid", 1) + ".runtime-test.svc.cluster.local"
	engine := &computev1alpha1.FireboltEngine{ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: "runtime-test", UID: "runtime-engine-uid"}}
	certificate := buildGenEngineTLSCertificate(engine.Name, engine.Namespace, 1, &ResolvedEngineTLSInfo{
		CertManager: &computev1alpha1.CertManagerSpec{
			IssuerRef: computev1alpha1.CertManagerIssuerRef{Name: "test-ca"}, Algorithm: "ECDSA", Size: 384,
		},
	})
	if err := bindEngineTLSRoutingAuthority(engine, certificate); err != nil {
		t.Fatal(err)
	}
	engineCertificate, _, _ := ca.leaf(t, certificate.Spec.DNSNames[0], 2, certificate.Spec.DNSNames[1:]...)
	_, gatewayCertPEM, gatewayKeyPEM := ca.leaf(t, "gateway.test", 3)
	dir := t.TempDir()
	for name, data := range map[string][]byte{"ca.crt": ca.pem, "tls.crt": gatewayCertPEM, "tls.key": gatewayKeyPEM} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "fixture", Namespace: "runtime-test"},
		Spec:       computev1alpha1.FireboltInstanceSpec{TLS: &computev1alpha1.TLSSpec{Gateway: &computev1alpha1.TLSListenerSpec{Enabled: true}}},
		Status: computev1alpha1.FireboltInstanceStatus{
			EngineTLS:  &computev1alpha1.EngineTLSStatus{Reencrypting: true},
			GatewayTLS: &computev1alpha1.GatewayTLSStatus{SecretName: "gateway-tls"},
		},
	}
	upstream := strings.ReplaceAll(buildDFPUpstreamTLSTransportSocket(instance), gatewayEngineCAMountPath, dir)
	downstream := strings.ReplaceAll(buildListenerDownstreamTLSTransportSocket(instance), gatewayTLSMountPath, dir)
	f := startRuntimeFixture(t, true, func(config map[string]any) {
		dfpCluster(t, config)["transport_socket"] = routingTLSTransport(t, upstream)
		for _, raw := range config["static_resources"].(map[string]any)["listeners"].([]any) {
			listener := raw.(map[string]any)
			if listener["name"] != "listener" {
				continue
			}
			for _, chain := range listener["filter_chains"].([]any) {
				chain.(map[string]any)["transport_socket"] = routingTLSTransport(t, downstream)
			}
		}
	})
	port := f.old.server.Listener.Addr().(*net.TCPAddr).Port
	f.authority = net.JoinHostPort(generationDNS, strconv.Itoa(port))
	f.query = strings.Replace(f.query, "http://", "https://", 1)
	var probes, queries, plaintext, wrongAuthority atomic.Int64
	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.TLS == nil {
			plaintext.Add(1)
		}
		if request.URL.Path == "/health/ready" {
			probes.Add(1)
			w.WriteHeader(http.StatusOK)
			return
		}
		if request.Host != f.authority {
			wrongAuthority.Add(1)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if string(body) != "SELECT 1" {
			http.Error(w, "unexpected query", http.StatusBadRequest)
			return
		}
		queries.Add(1)
		_, _ = io.WriteString(w, "1") // Client disconnects during teardown are harmless.
	}))
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", net.JoinHostPort("127.0.0.4", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	backend.Listener = listener
	backend.TLS = &tls.Config{Certificates: []tls.Certificate{engineCertificate}, MinVersion: tls.VersionTLS12}
	backend.StartTLS()
	t.Cleanup(backend.Close)
	f.dns.set([]string{"127.0.0.4"}, dnsmessage.RCodeSuccess)
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca.pem) {
		t.Fatal("invalid test CA")
	}
	clientTLS := &tls.Config{RootCAs: roots, ServerName: "gateway.test", MinVersion: tls.VersionTLS12}
	f.client.Transport = &http.Transport{TLSClientConfig: clientTLS.Clone(), DisableKeepAlives: true}
	probe, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.probe+"/health/ready", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	probe.Host = f.authority
	response, err := f.client.Do(probe)
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.Copy(io.Discard, response.Body)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("encrypted generation probe: status=%d read=%v close=%v", response.StatusCode, readErr, closeErr)
	}
	f.probeGate.Store(true)
	for _, protocol := range []string{"HTTP/1.1", "HTTP/2"} {
		t.Run(protocol, func(t *testing.T) {
			var transport http.RoundTripper
			if protocol == "HTTP/2" {
				h2 := &http2.Transport{TLSClientConfig: clientTLS.Clone()}
				t.Cleanup(h2.CloseIdleConnections)
				transport = h2
			} else {
				h1 := &http.Transport{TLSClientConfig: clientTLS.Clone(), DisableKeepAlives: true}
				t.Cleanup(h1.CloseIdleConnections)
				transport = h1
			}
			client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.query, strings.NewReader("SELECT 1"))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("X-Firebolt-Engine", "probe")
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			if closeErr := response.Body.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			if err != nil || response.StatusCode != http.StatusOK || string(body) != "1" {
				t.Fatalf("query: status=%d body=%q error=%v", response.StatusCode, body, err)
			}
			if response.TLS == nil || (protocol == "HTTP/2" && response.ProtoMajor != 2) {
				t.Fatalf("unexpected downstream transport: TLS=%v protocol=%s", response.TLS != nil, response.Proto)
			}
		})
	}
	if queries.Load() != 2 || probes.Load() < 3 || plaintext.Load() != 0 || wrongAuthority.Load() != 0 {
		t.Fatalf("TLS routing observations: queries=%d probes=%d plaintext=%d wrongAuthority=%d", queries.Load(), probes.Load(), plaintext.Load(), wrongAuthority.Load())
	}
	if len(f.old.received()) != 0 || len(f.new.received()) != 0 {
		t.Fatal("query escaped the immutable TLS destination")
	}
}
