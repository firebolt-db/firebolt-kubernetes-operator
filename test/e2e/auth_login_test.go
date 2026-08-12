//go:build e2e

/*
Copyright 2026 Firebolt Analytics.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/controller"
)

// This suite exercises the login path end to end against a real engine: the
// embedded authorization server issues a JWT for the operator-provisioned admin
// credential, signed by the operator-provisioned key, and the engine accepts it
// on a query. auth_tls_test.go covers the transport; nothing there ever obtained
// a token, so the design's central claim — every engine in an Instance runs
// byte-identical signing keys, so a token minted by one validates on another —
// went untested.
//
// The contract is packdb's specs/authentication.md §8.2 and §10.13: POST
// /oauth/token, grant_type=client_credentials, username as client_id, password
// as client_secret, and a REQUIRED RFC 8707 resource parameter naming the
// Instance.

// newTestSecretValue returns a fresh random value for use as a test credential.
//
// Generated rather than hard-coded: a literal password in the source trips
// secret scanning, and nothing in these specs depends on a fixed value.
func newTestSecretValue() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("generating a test credential: %v", err))
	}
	return hex.EncodeToString(b)
}

// instanceResource is the Instance's protected-resource identifier, the only
// accepted `resource` value and the `aud` every minted token carries.
func instanceResource(instanceID string) string {
	return "https://firebolt.io/instance/" + instanceID
}

// curlHTTP runs curl inside a pod and returns the response body and HTTP status
// without failing on 4xx/5xx, so negative cases can assert the OAuth error
// grammar rather than only that something went wrong.
//
// The status rides stderr via -w %{stderr}, keeping the body on stdout intact
// for JSON decoding.
func curlHTTP(ctx context.Context, pod, caPath string, curlArgs ...string) (body string, status int, err error) {
	args := []string{"-sS", "--connect-timeout", "5", "--max-time", "30",
		"-w", "%{stderr}status=%{http_code}\n"}
	if caPath != "" {
		args = append(args, "--cacert", caPath)
	}
	args = append(args, curlArgs...)

	kargs := kubectlArgs("exec", pod, "-n", testNamespace, "--", "curl")
	kargs = append(kargs, args...)
	cmd := exec.CommandContext(ctx, "kubectl", kargs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	execErr := cmd.Run()

	// -sS keeps curl's own diagnostics on stderr alongside the status line, so
	// stderr must be captured whether or not curl exited non-zero: a 4xx is a
	// successful exchange this helper is expected to report, not a failure.
	for _, line := range strings.Split(stderr.String(), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "status="); ok {
			status, _ = strconv.Atoi(rest)
		}
	}
	if status == 0 {
		return stdout.String(), 0, fmt.Errorf("curl did not complete (%w): %s",
			execErr, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), status, nil
}

// fireboltDiscovery is the subset of /.well-known/firebolt this suite reads.
// Field names follow packdb's specs/metadata-discovery.md §3.
type fireboltDiscovery struct {
	Instance struct {
		ID   string `json:"id"`
		Auth *struct {
			OAuth struct {
				ProtectedResource struct {
					Resource string `json:"resource"`
				} `json:"protectedResource"`
				AuthorizationServers []struct {
					Name          string `json:"name"`
					Issuer        string `json:"issuer"`
					TokenEndpoint string `json:"token_endpoint"`
					JWKSURI       string `json:"jwks_uri"`
				} `json:"authorizationServers"`
			} `json:"oauth"`
		} `json:"auth"`
	} `json:"instance"`
}

// fetchDiscovery reads the Firebolt Metadata document from an engine.
func fetchDiscovery(ctx context.Context, pod, baseURL, caPath string) (*fireboltDiscovery, error) {
	body, status, err := curlHTTP(ctx, pod, caPath, baseURL+"/.well-known/firebolt")
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("GET /.well-known/firebolt: status %d, body %s", status, body)
	}
	var doc fireboltDiscovery
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		return nil, fmt.Errorf("decoding discovery document %q: %w", body, err)
	}
	return &doc, nil
}

// tokenResponse is the OAuth 2.0 token endpoint's response (RFC 6749 §5.1) and,
// on failure, its error body (§5.2).
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Error       string `json:"error"`
	ErrorDesc   string `json:"error_description"`
}

// requestToken runs the client_credentials exchange against an engine's embedded
// authorization server. resource is passed verbatim (empty omits it) so the
// negative cases can exercise the RFC 8707 rules.
func requestToken(ctx context.Context, pod, baseURL, caPath, user, pass, resource string) (*tokenResponse, int, error) {
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {user},
		"client_secret": {pass},
	}
	if resource != "" {
		form.Set("resource", resource)
	}
	body, status, err := curlHTTP(ctx, pod, caPath,
		"-X", "POST",
		"-H", "Content-Type: application/x-www-form-urlencoded",
		"--data", form.Encode(),
		baseURL+"/oauth/token")
	if err != nil {
		return nil, status, err
	}
	var tr tokenResponse
	if jsonErr := json.Unmarshal([]byte(body), &tr); jsonErr != nil {
		return nil, status, fmt.Errorf("decoding token response (status %d) %q: %w", status, body, jsonErr)
	}
	return &tr, status, nil
}

// decodeJWTSegment decodes one base64url JWT segment into a generic map. Written
// out rather than pulled from a JOSE library: the suite only inspects the header
// and claims, and never verifies the signature (the engine accepting the token
// is the real signature check).
func decodeJWTSegment(seg string) (map[string]any, error) {
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// decodeJWT splits a compact JWS and decodes its header and claims.
func decodeJWT(token string) (header, claims map[string]any, err error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, nil, fmt.Errorf("not a compact JWS (%d segments)", len(parts))
	}
	if header, err = decodeJWTSegment(parts[0]); err != nil {
		return nil, nil, fmt.Errorf("decoding JWT header: %w", err)
	}
	if claims, err = decodeJWTSegment(parts[1]); err != nil {
		return nil, nil, fmt.Errorf("decoding JWT claims: %w", err)
	}
	return header, claims, nil
}

// jwksKids lists the key IDs an engine publishes at /oauth/jwks — what it will
// actually validate against, as opposed to what the operator believes it rendered.
func jwksKids(ctx context.Context, pod, baseURL, caPath string) ([]string, error) {
	body, status, err := curlHTTP(ctx, pod, caPath, baseURL+"/oauth/jwks")
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("GET /oauth/jwks: status %d, body %s", status, body)
	}
	var jwks struct {
		Keys []struct {
			Kid string `json:"kid"`
		} `json:"keys"`
	}
	if err := json.Unmarshal([]byte(body), &jwks); err != nil {
		return nil, fmt.Errorf("decoding JWKS %q: %w", body, err)
	}
	kids := make([]string, 0, len(jwks.Keys))
	for _, k := range jwks.Keys {
		kids = append(kids, k.Kid)
	}
	return kids, nil
}

// runAuthedQuery POSTs query with a bearer token and returns the body and HTTP
// status.
//
// execCurlQuery cannot serve here: it has no --cacert option, so it cannot
// verify a privately-issued engine certificate — which is the whole point of
// these specs. It also uses -f, which would collapse an auth rejection into a
// generic curl failure instead of a status this suite can assert on.
func runAuthedQuery(ctx context.Context, pod, baseURL, caPath, token, query string, extraHeaders ...string) (string, int, error) {
	args := []string{
		"-X", "POST",
		"-H", "Content-Type: text/plain",
		"-H", "Authorization: Bearer " + token,
	}
	for _, h := range extraHeaders {
		args = append(args, "-H", h)
	}
	args = append(args, "--data", query, baseURL+"/?output_format=JSON_Compact")
	return curlHTTP(ctx, pod, caPath, args...)
}

// engineQueryURL is an engine's routing Service over TLS. The Service FQDN is
// covered by the engine certificate's namespace-wide wildcard SAN, so a
// verifying curl works against it.
func engineQueryURL(engineName string) string {
	return fmt.Sprintf("https://%s-service.%s.svc.cluster.local:%d",
		engineName, testNamespace, controller.EngineHTTPQueryPort)
}

// instanceAuthStatus reads the Instance's provisioned signing keys.
func instanceAuthStatus(ctx context.Context, name string) (*computev1alpha1.AuthStatus, error) {
	cl, err := getCRDClient()
	if err != nil {
		return nil, err
	}
	inst := &computev1alpha1.FireboltInstance{}
	if err := cl.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, inst); err != nil {
		return nil, err
	}
	if inst.Status.Auth == nil {
		return nil, fmt.Errorf("FireboltInstance %q has no status.auth", name)
	}
	return inst.Status.Auth, nil
}

// activeSigningKeyID returns the kid the Instance currently signs with.
func activeSigningKeyID(auth *computev1alpha1.AuthStatus) string {
	for _, k := range auth.SigningKeys {
		if k.Phase == computev1alpha1.SigningKeyActive {
			return k.ID
		}
	}
	return ""
}

var _ = Describe("FireboltInstance auth login", func() {
	Describe("the embedded authorization server issues tokens engines accept", Ordered, func() {
		const (
			instanceName    = "inst-authlogin"
			engineA         = "authlogin-a"
			engineB         = "authlogin-b"
			clientPod       = "client-authlogin"
			adminSecretName = "inst-authlogin-admin"
			adminSecretKey  = "password"
			adminUser       = "firebolt"
			caPodPath       = "/tmp/e2e-ca.crt"
		)
		var (
			lc            *TestInstanceLifecycle
			instanceID    string
			resource      string
			adminPassword = newTestSecretValue()
		)

		instName := instanceName
		engName := engineA
		RegisterFailedSpecPodLogDump(&instName, &engName)

		BeforeAll(func() {
			By("creating the admin-password Secret")
			Expect(createAdminSecret(ctx, adminSecretName, adminSecretKey, adminPassword)).To(Succeed())

			By("creating an auth+TLS FireboltInstance and starting its operators")
			var err error
			lc, err = SetupTestInstanceWithMutate(ctx, instanceName, func(inst *computev1alpha1.FireboltInstance) {
				inst.Spec.Auth = &computev1alpha1.AuthSpec{
					Enabled: true,
					Local: &computev1alpha1.LocalAuthSpec{
						Admin: computev1alpha1.AdminSpec{
							Name: adminUser,
							Password: corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: adminSecretName},
								Key:                  adminSecretKey,
							},
						},
						SigningAlgorithm: "ES384",
						SigningKeys: &computev1alpha1.SigningKeyPolicy{
							CertManager: computev1alpha1.CertManagerSpec{
								IssuerRef: caClusterIssuerRef(), Algorithm: "ECDSA", Size: 384,
							},
						},
					},
				}
				inst.Spec.TLS = &computev1alpha1.TLSSpec{
					Engine: &computev1alpha1.TLSListenerSpec{
						Enabled:     true,
						CertManager: &computev1alpha1.CertManagerSpec{IssuerRef: caClusterIssuerRef(), Algorithm: "ECDSA", Size: 384},
					},
					Gateway: &computev1alpha1.TLSListenerSpec{
						Enabled:     true,
						CertManager: &computev1alpha1.CertManagerSpec{IssuerRef: caClusterIssuerRef(), Algorithm: "ECDSA", Size: 384},
					},
				}
			})
			Expect(err).NotTo(HaveOccurred())

			By("waiting for AuthReady")
			Expect(waitForInstanceCondition(ctx, instanceName,
				computev1alpha1.InstanceConditionAuthReady, metav1.ConditionTrue)).To(Succeed())

			By("creating a client pod with the suite CA installed")
			// Engine and gateway TLS readiness are waited on after the engines
			// exist — see below. Both conditions are convergence-gated, and the
			// engine-TLS one cannot go True before there is a fleet to converge.
			Expect(CreateClientPod(ctx, clientPod)).To(Succeed())
			ca, err := engineTrustCAPEM(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(writeFileInPod(ctx, clientPod, caPodPath, ca)).To(Succeed())

			By("creating two engines in the same Instance")
			// Two engines, not two pods of one engine: the engine certificate's
			// SANs are the namespace wildcard and localhost, and a one-label
			// wildcard does not cover per-pod StatefulSet DNS — so a verifying
			// client cannot address an individual pod, but can address each
			// engine's own routing Service.
			for _, name := range []string{engineA, engineB} {
				Expect(CreateEngine(ctx, instanceName, name, 1)).To(Succeed())
			}
			for _, name := range []string{engineA, engineB} {
				Expect(WaitForEngineReady(ctx, name, 1, clusterReadyTimeout)).To(Succeed())
				Expect(WaitForEngineStable(ctx, name, clusterTransitionTimeout)).To(Succeed())
			}

			By("waiting for the fleet to converge on TLS before routing through the gateway")
			// EngineTLSReady is convergence-gated: it turns True only once every
			// engine serves TLS and the gateway has switched its upstream to match.
			// The gateway's upstream protocol is fleet-wide, so querying before this
			// point gets a 503 from a gateway still speaking plaintext to
			// TLS-only engines — a race in the test, not a defect in the operator.
			for _, cond := range []string{
				computev1alpha1.InstanceConditionEngineTLSReady,
				computev1alpha1.InstanceConditionGatewayTLSReady,
			} {
				Expect(waitForInstanceCondition(ctx, instanceName, cond, metav1.ConditionTrue)).To(Succeed())
			}

			By("reading the Instance ID the token audience is bound to")
			cl, err := getCRDClient()
			Expect(err).NotTo(HaveOccurred())
			inst := &computev1alpha1.FireboltInstance{}
			Expect(cl.Get(ctx, types.NamespacedName{Name: instanceName, Namespace: testNamespace}, inst)).To(Succeed())
			Expect(inst.Spec.ID).NotTo(BeEmpty())
			instanceID = inst.Spec.ID
			resource = instanceResource(instanceID)

			By("asserting the engine advertises the embedded authorization server")
			// A hard assertion, not a Skip: the engine tag is pinned and the
			// embedded AS is present in it, so a failure here means someone moved
			// the tag past auth support and should hear about it loudly rather
			// than watch this whole Describe quietly stop testing anything.
			doc, err := fetchDiscovery(ctx, clientPod, engineQueryURL(engineA), caPodPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(doc.Instance.Auth).NotTo(BeNil(),
				"instance.auth is null — the engine reports authentication disabled")
			Expect(doc.Instance.ID).To(Equal(instanceID))
			Expect(doc.Instance.Auth.OAuth.ProtectedResource.Resource).To(Equal(resource))
			names := []string{}
			for _, as := range doc.Instance.Auth.OAuth.AuthorizationServers {
				names = append(names, as.Name)
			}
			Expect(names).To(ContainElement("_local"),
				"the embedded authorization server is not advertised")
		})

		AfterAll(func() {
			DeleteClientPod(ctx, clientPod)
			for _, name := range []string{engineA, engineB} {
				if err := DeleteEngine(ctx, name); err != nil {
					fmt.Fprintf(GinkgoWriter, "AfterAll: DeleteEngine(%s): %v\n", name, err)
				}
			}
			TeardownTestInstance(ctx, lc)
			_ = k8sClient.CoreV1().Secrets(testNamespace).Delete(ctx, adminSecretName, metav1.DeleteOptions{})
		})

		It("rejects a query carrying no credentials", func() {
			_, status, err := curlHTTP(ctx, clientPod, caPodPath,
				"-X", "POST", "--data", "SELECT 1", engineQueryURL(engineA)+"/?output_format=JSON_Compact")
			Expect(err).NotTo(HaveOccurred())
			Expect(status).To(Equal(401),
				"an unauthenticated query must be rejected once auth is enabled")
		})

		It("issues a token signed by the operator-provisioned key", func() {
			tr, status, err := requestToken(ctx, clientPod, engineQueryURL(engineA), caPodPath,
				adminUser, adminPassword, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(status).To(Equal(200), "token request failed: %+v", tr)
			Expect(tr.AccessToken).NotTo(BeEmpty())

			header, claims, err := decodeJWT(tr.AccessToken)
			Expect(err).NotTo(HaveOccurred())

			By("the signing kid matches the Instance's Active signing key")
			// This is the assertion that ties operator provisioning to what the
			// engine actually signs with: the key cert-manager issued, that the
			// operator promoted to Active and rendered, is the one in use.
			auth, err := instanceAuthStatus(ctx, instanceName)
			Expect(err).NotTo(HaveOccurred())
			Expect(header["kid"]).To(Equal(activeSigningKeyID(auth)))

			By("iss and aud name the Instance")
			Expect(claims["iss"]).To(Equal(resource))
			Expect(claims["aud"]).To(SatisfyAny(Equal(resource), ContainElement(resource)))
		})

		It("accepts a query bearing that token", func() {
			tr, status, err := requestToken(ctx, clientPod, engineQueryURL(engineA), caPodPath,
				adminUser, adminPassword, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(status).To(Equal(200))

			body, qStatus, err := runAuthedQuery(ctx, clientPod, engineQueryURL(engineA), caPodPath,
				tr.AccessToken, "SELECT 1")
			Expect(err).NotTo(HaveOccurred())
			Expect(qStatus).To(Equal(200), "authenticated query rejected: %s", body)
			Expect(body).To(ContainSubstring("1"))
		})

		It("validates a token minted by one engine on a different engine", func() {
			// The design's central claim: auth is Instance-wide, so every engine
			// renders byte-identical signing keys. If the operator ever let two
			// engines diverge, this is the spec that fails.
			tr, status, err := requestToken(ctx, clientPod, engineQueryURL(engineA), caPodPath,
				adminUser, adminPassword, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(status).To(Equal(200))

			body, qStatus, err := runAuthedQuery(ctx, clientPod, engineQueryURL(engineB), caPodPath,
				tr.AccessToken, "SELECT 2")
			Expect(err).NotTo(HaveOccurred())
			Expect(qStatus).To(Equal(200),
				"a token minted by %s was rejected by %s — the fleet's signing keys have diverged: %s",
				engineA, engineB, body)
			Expect(body).To(ContainSubstring("2"))
		})

		It("publishes exactly the signing keys the operator rendered", func() {
			auth, err := instanceAuthStatus(ctx, instanceName)
			Expect(err).NotTo(HaveOccurred())
			var want []string
			for _, k := range auth.SigningKeys {
				if k.Phase != computev1alpha1.SigningKeyRemoving {
					want = append(want, k.ID)
				}
			}
			Expect(want).NotTo(BeEmpty())

			kids, err := jwksKids(ctx, clientPod, engineQueryURL(engineA), caPodPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(kids).To(ConsistOf(want),
				"the engine's JWKS does not match the keys the operator rendered")
		})

		It("rejects bad credentials and a mismatched resource", func() {
			// The OAuth error code is what is asserted strictly: it is the part of
			// the contract clients key off, and it is what distinguishes these three
			// rejections from one another. The status is only required to be a
			// client error — RFC 6749 §5.2 mandates 400 in general but explicitly
			// permits 401 for invalid_client, and the engine uses 401 there.
			clientError := SatisfyAny(Equal(400), Equal(401))

			By("a wrong password is invalid_client")
			tr, status, err := requestToken(ctx, clientPod, engineQueryURL(engineA), caPodPath,
				adminUser, newTestSecretValue(), resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(status).To(clientError)
			Expect(tr.Error).To(Equal("invalid_client"))

			By("omitting the required resource parameter is invalid_request")
			tr, status, err = requestToken(ctx, clientPod, engineQueryURL(engineA), caPodPath,
				adminUser, adminPassword, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(status).To(clientError)
			Expect(tr.Error).To(Equal("invalid_request"))

			By("a resource naming another Instance is invalid_target")
			tr, status, err = requestToken(ctx, clientPod, engineQueryURL(engineA), caPodPath,
				adminUser, adminPassword, instanceResource("01ARZ3NDEKTSV4RRFFQ69G5FAV"))
			Expect(err).NotTo(HaveOccurred())
			Expect(status).To(clientError)
			Expect(tr.Error).To(Equal("invalid_target"))
		})

		It("carries the token through the gateway to an engine", func() {
			// Proves the gateway forwards Authorization untouched while
			// re-encrypting upstream to a TLS-serving engine. The token is minted
			// directly against the engine and only spent through the gateway, so a
			// failure here is unambiguously about the gateway's request path.
			//
			// The gateway's client-facing port stays 80 when TLS is enabled — TLS
			// replaces plaintext on the same listener — and the Service FQDN is one
			// of the gateway certificate's SANs, so a verifying client works.
			gwURL := fmt.Sprintf("https://%s%s.%s.svc.cluster.local:80",
				instanceName, controller.SuffixGateway, testNamespace)

			tr, status, err := requestToken(ctx, clientPod, engineQueryURL(engineA), caPodPath,
				adminUser, adminPassword, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(status).To(Equal(200), "token request failed: %+v", tr)

			body, qStatus, err := runAuthedQuery(ctx, clientPod, gwURL, caPodPath,
				tr.AccessToken, "SELECT 3", "X-Firebolt-Engine: "+engineA)
			Expect(err).NotTo(HaveOccurred())
			Expect(qStatus).To(Equal(200), "query through the gateway failed: %s", body)
			Expect(body).To(ContainSubstring("3"))
		})
	})

	// Labelled so it can be excluded: every gate in the rotation waits for a full
	// engine roll, which makes this the slowest spec in the suite by a wide margin.
	Describe("signing-key rotation keeps the fleet able to validate tokens", Ordered,
		Label("e2e", "signing-key-rotation", "slow"), func() {
			const (
				instanceName    = "inst-authrot"
				engineName      = "authrot-engine"
				clientPod       = "client-authrot"
				adminSecretName = "inst-authrot-admin"
				adminSecretKey  = "password"
				adminUser       = "firebolt"
				caPodPath       = "/tmp/e2e-ca.crt"
				// The rotation cadence.
				//
				// maxTokenAge must exceed how long a mint-and-promote takes, because
				// it bounds a token's usable life as well as the retain floor: pinned
				// too low, the pre-rotation token this spec carries across the
				// promotion ages out mid-rotation and is rejected as invalid_token
				// before the retain window is ever exercised.
				//
				// retainDuration is floored at maxTokenAge + clockSkewTolerance
				// (ValidateAuth), which is precisely why it cannot be short: a token
				// signed the instant a key is demoted stays valid that long, so the
				// key cannot be pruned sooner. A consequence is that the demoted key
				// will not be removed within this spec's lifetime — see the second It.
				rotationInterval = time.Minute
				retainDuration   = 20 * time.Minute
				maxTokenAge      = "15m"
				clockSkew        = "0s"
				// Each convergence gate waits for a full engine roll, and
				// clusterReadyTimeout is the budget for a single roll — so a
				// mint-plus-promote (two rolls) needs several times that.
				rotationStepTimeout = 10 * time.Minute
			)
			var (
				lc            *TestInstanceLifecycle
				resource      string
				adminPassword = newTestSecretValue()
			)

			instName := instanceName
			engName := engineName
			RegisterFailedSpecPodLogDump(&instName, &engName)

			BeforeAll(func() {
				Expect(createAdminSecret(ctx, adminSecretName, adminSecretKey, adminPassword)).To(Succeed())

				var err error
				lc, err = SetupTestInstanceWithMutate(ctx, instanceName, func(inst *computev1alpha1.FireboltInstance) {
					inst.Spec.Auth = &computev1alpha1.AuthSpec{
						Enabled: true,
						Local: &computev1alpha1.LocalAuthSpec{
							Admin: computev1alpha1.AdminSpec{
								Name: adminUser,
								Password: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: adminSecretName},
									Key:                  adminSecretKey,
								},
							},
							SigningAlgorithm:   "ES384",
							MaxTokenAge:        maxTokenAge,
							ClockSkewTolerance: clockSkew,
							SigningKeys: &computev1alpha1.SigningKeyPolicy{
								RotationInterval: &metav1.Duration{Duration: rotationInterval},
								RetainDuration:   &metav1.Duration{Duration: retainDuration},
								CertManager: computev1alpha1.CertManagerSpec{
									IssuerRef: caClusterIssuerRef(), Algorithm: "ECDSA", Size: 384,
								},
							},
						},
					}
					inst.Spec.TLS = &computev1alpha1.TLSSpec{
						Engine: &computev1alpha1.TLSListenerSpec{
							Enabled:     true,
							CertManager: &computev1alpha1.CertManagerSpec{IssuerRef: caClusterIssuerRef(), Algorithm: "ECDSA", Size: 384},
						},
					}
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(waitForInstanceCondition(ctx, instanceName,
					computev1alpha1.InstanceConditionAuthReady, metav1.ConditionTrue)).To(Succeed())

				Expect(CreateClientPod(ctx, clientPod)).To(Succeed())
				ca, err := engineTrustCAPEM(ctx)
				Expect(err).NotTo(HaveOccurred())
				Expect(writeFileInPod(ctx, clientPod, caPodPath, ca)).To(Succeed())

				Expect(CreateEngine(ctx, instanceName, engineName, 1)).To(Succeed())
				Expect(WaitForEngineReady(ctx, engineName, 1, clusterReadyTimeout)).To(Succeed())
				Expect(WaitForEngineStable(ctx, engineName, clusterTransitionTimeout)).To(Succeed())

				cl, err := getCRDClient()
				Expect(err).NotTo(HaveOccurred())
				inst := &computev1alpha1.FireboltInstance{}
				Expect(cl.Get(ctx, types.NamespacedName{Name: instanceName, Namespace: testNamespace}, inst)).To(Succeed())
				resource = instanceResource(inst.Spec.ID)
			})

			AfterAll(func() {
				DeleteClientPod(ctx, clientPod)
				if err := DeleteEngine(ctx, engineName); err != nil {
					fmt.Fprintf(GinkgoWriter, "AfterAll: DeleteEngine(%s): %v\n", engineName, err)
				}
				TeardownTestInstance(ctx, lc)
				_ = k8sClient.CoreV1().Secrets(testNamespace).Delete(ctx, adminSecretName, metav1.DeleteOptions{})
			})

			It("keeps a pre-rotation token valid across the promotion", func() {
				By("minting a token under the original key")
				tr, status, err := requestToken(ctx, clientPod, engineQueryURL(engineName), caPodPath,
					adminUser, adminPassword, resource)
				Expect(err).NotTo(HaveOccurred())
				Expect(status).To(Equal(200), "token request failed: %+v", tr)
				oldToken := tr.AccessToken
				oldHeader, _, err := decodeJWT(oldToken)
				Expect(err).NotTo(HaveOccurred())
				originalKid := oldHeader["kid"]

				By("waiting for the operator to mint and promote a second key")
				// mint -> converge -> promote, each step one reconcile apart and each
				// gated on the engine reporting the new authHash, so this legitimately
				// takes several engine rolls.
				Eventually(func(g Gomega) {
					auth, err := instanceAuthStatus(ctx, instanceName)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(auth.SigningKeyGeneration).To(BeNumerically(">=", 2))
					g.Expect(activeSigningKeyID(auth)).NotTo(Equal(originalKid),
						"generation 2 exists but has not been promoted to Active")
				}).WithTimeout(rotationStepTimeout).WithPolling(5 * time.Second).Should(Succeed())

				By("both kids are published while the demoted key is retained")
				Eventually(func(g Gomega) {
					kids, err := jwksKids(ctx, clientPod, engineQueryURL(engineName), caPodPath)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(kids).To(ContainElement(originalKid),
						"the demoted key left the JWKS before its retain window elapsed")
					g.Expect(len(kids)).To(BeNumerically(">=", 2))
				}).WithTimeout(rotationStepTimeout).WithPolling(5 * time.Second).Should(Succeed())

				By("a token signed before the promotion still validates")
				// The whole point of the retain window: the demoted key keeps
				// validating tokens it already signed.
				retained, qStatus, err := runAuthedQuery(ctx, clientPod, engineQueryURL(engineName), caPodPath,
					oldToken, "SELECT 1")
				Expect(err).NotTo(HaveOccurred())
				Expect(qStatus).To(Equal(200),
					"a token signed before the rotation was rejected during the retain window: %s", retained)

				By("a freshly minted token eventually carries the new kid")
				// Eventually, not immediately: promotion flips the Active key in the
				// Instance's status, but each engine keeps signing with the demoted
				// key until it rolls onto the promoted config. That lag is the whole
				// reason RetireEligibleAt anchors the retain window at convergence
				// rather than at demotion, so a test that demanded the new kid the
				// instant status changed would be asserting against the design.
				Eventually(func(g Gomega) {
					tr, status, err := requestToken(ctx, clientPod, engineQueryURL(engineName), caPodPath,
						adminUser, adminPassword, resource)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(status).To(Equal(200))
					newHeader, _, err := decodeJWT(tr.AccessToken)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(newHeader["kid"]).NotTo(Equal(originalKid))
				}).WithTimeout(rotationStepTimeout).WithPolling(5 * time.Second).Should(Succeed())
			})

			It("does not retire the demoted key before its retain window elapses", func() {
				// The safety-relevant direction. Retiring a key EARLY breaks tokens
				// that are still valid; retiring it late costs nothing, so that is the
				// property worth an end-to-end guard.
				//
				// Full retirement is deliberately not asserted here: retainDuration is
				// floored at maxTokenAge, which this spec must set above the rotation's
				// own duration, so waiting out the window would add tens of minutes to
				// the suite. The removal steps and their convergence gates are covered
				// by the unit tests and by formal/SigningKeyRotation.tla.
				auth, err := instanceAuthStatus(ctx, instanceName)
				Expect(err).NotTo(HaveOccurred())
				Expect(auth.SigningKeys).To(HaveLen(2),
					"the demoted key was dropped from status while its retain window was still open")

				var demoted *computev1alpha1.SigningKeyStatus
				for i := range auth.SigningKeys {
					if auth.SigningKeys[i].Phase == computev1alpha1.SigningKeyValidationOnly {
						demoted = &auth.SigningKeys[i]
					}
				}
				Expect(demoted).NotTo(BeNil(), "no demoted key found after a promotion")
				Expect(demoted.DemotedAt).NotTo(BeNil())
				Expect(demoted.Phase).NotTo(Equal(computev1alpha1.SigningKeyRemoving),
					"the demoted key entered Removing before its retain window elapsed")

				By("the engine still publishes it for validation")
				kids, err := jwksKids(ctx, clientPod, engineQueryURL(engineName), caPodPath)
				Expect(err).NotTo(HaveOccurred())
				Expect(kids).To(ContainElement(demoted.ID))
			})
		})
})
