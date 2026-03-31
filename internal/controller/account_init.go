package controller

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	adminv2 "github.com/firebolt-analytics/pensieve/gen/proto/go/admin/v2"
	transactionv1 "github.com/firebolt-analytics/pensieve/gen/proto/go/transaction/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	computev1alpha1 "github.com/firebolt-analytics/core-operator/api/v1alpha1"
)

// ensureAccountInitialized checks that the metadata service has exactly one
// active account.  If zero accounts exist it creates and activates one.
// The resolved account ID is returned so the caller can inject it into the
// engine configuration.
func (r *FireboltEngineReconciler) ensureAccountInitialized(ctx context.Context, engine *computev1alpha1.FireboltEngine) (string, error) {
	log := logf.FromContext(ctx)

	conn, cleanup, err := r.dialMetadataService(ctx, engine)
	if err != nil {
		return "", fmt.Errorf("dial metadata service: %w", err)
	}
	defer cleanup()

	txClient := transactionv1.NewTransactionServiceClient(conn)
	adminClient := adminv2.NewAdminServiceClient(conn)

	// --- Check existing accounts with a read-only admin transaction ---

	roResp, err := txClient.StartReadOnlyAdminTransaction(ctx, &transactionv1.StartReadOnlyAdminTransactionRequest{})
	if err != nil {
		return "", fmt.Errorf("StartReadOnlyAdminTransaction: %w", err)
	}
	roTxID := roResp.GetAdminTransactionId()

	accounts, states, err := listAllAccounts(ctx, adminClient, roTxID)
	if err != nil {
		return "", fmt.Errorf("GetAccounts: %w", err)
	}

	switch len(accounts) {
	case 1:
		if states[0] != adminv2.AccountState_ACCOUNT_STATE_ACTIVE {
			return "", fmt.Errorf("metadata service account %s exists but is not active (state=%s); manual intervention required",
				accounts[0], states[0])
		}
		log.Info("Metadata service account already active", "accountId", accounts[0])
		return accounts[0], nil

	case 0:
		log.Info("No accounts found in metadata service, creating initial account")
		accountID, err := createAndProvisionAccount(ctx, txClient, adminClient)
		if err != nil {
			return "", fmt.Errorf("failed to create initial account: %w", err)
		}
		log.Info("Initial metadata service account created and activated", "accountId", accountID)
		return accountID, nil

	default:
		return "", fmt.Errorf("metadata service has %d accounts; expected exactly 1; manual intervention required", len(accounts))
	}
}

// dialMetadataService establishes a gRPC connection to the metadata service.
// It first tries the in-cluster DNS name; if that fails (e.g. the operator
// is running outside the cluster during E2E tests), it falls back to a
// Kubernetes port-forward through the API server.
func (r *FireboltEngineReconciler) dialMetadataService(ctx context.Context, engine *computev1alpha1.FireboltEngine) (*grpc.ClientConn, func(), error) {
	endpoint := MetadataServiceEndpoint(engine.Name, engine.Namespace)

	// Quick DNS probe to decide whether in-cluster resolution works.
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	host, _, _ := net.SplitHostPort(endpoint)
	addrs, _ := net.DefaultResolver.LookupHost(probeCtx, host)
	if len(addrs) > 0 {
		conn, err := grpc.NewClient(endpoint,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		if err != nil {
			return nil, nil, err
		}
		return conn, func() { conn.Close() }, nil
	}

	// Fallback: port-forward through the API server.
	return r.portForwardGRPC(ctx, engine)
}

// portForwardGRPC sets up a SPDY port-forward to one of the metadata service
// pods and returns a gRPC connection over the forwarded port.
func (r *FireboltEngineReconciler) portForwardGRPC(ctx context.Context, engine *computev1alpha1.FireboltEngine) (*grpc.ClientConn, func(), error) {
	log := logf.FromContext(ctx)
	ns := engine.Namespace
	deployName := metadataName(engine.Name)

	dep, err := r.Clientset.AppsV1().Deployments(ns).Get(ctx, deployName, metav1.GetOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("getting metadata deployment: %w", err)
	}
	selector, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing deployment selector: %w", err)
	}

	pods, err := r.Clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: selector.String(),
		FieldSelector: "status.phase=Running",
		Limit:         1,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("listing metadata pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return nil, nil, fmt.Errorf("no running pods found for deployment %s/%s", ns, deployName)
	}
	podName := pods.Items[0].Name

	// Allocate an ephemeral local port.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, fmt.Errorf("allocating local port: %w", err)
	}
	localPort := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	// Build SPDY round-tripper for the port-forward.
	reqURL := r.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(ns).
		Name(podName).
		SubResource("portforward").
		URL()

	transport, upgrader, err := spdy.RoundTripperFor(r.RestConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("creating SPDY transport: %w", err)
	}

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, reqURL)

	stopChan := make(chan struct{}, 1)
	readyChan := make(chan struct{})

	ports := []string{fmt.Sprintf("%d:%d", localPort, MetadataServicePort)}
	fw, err := portforward.New(dialer, ports, stopChan, readyChan, &bytes.Buffer{}, &bytes.Buffer{})
	if err != nil {
		return nil, nil, fmt.Errorf("creating port-forwarder: %w", err)
	}

	errChan := make(chan error, 1)
	go func() { errChan <- fw.ForwardPorts() }()

	select {
	case <-readyChan:
	case err := <-errChan:
		return nil, nil, fmt.Errorf("port-forward failed: %w", err)
	case <-time.After(15 * time.Second):
		close(stopChan)
		return nil, nil, fmt.Errorf("port-forward timed out for pod %s/%s", ns, podName)
	}

	log.V(1).Info("Port-forward to metadata service established", "pod", podName, "localPort", localPort)

	target := fmt.Sprintf("127.0.0.1:%d", localPort)
	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		close(stopChan)
		return nil, nil, fmt.Errorf("dialing port-forwarded endpoint: %w", err)
	}

	cleanup := func() {
		conn.Close()
		close(stopChan)
	}
	return conn, cleanup, nil
}

// listAllAccounts streams all accounts from GetAccounts and returns their IDs
// and states.
func listAllAccounts(ctx context.Context, client adminv2.AdminServiceClient, adminTxID string) (ids []string, states []adminv2.AccountState, err error) {
	stream, err := client.GetAccounts(ctx, &adminv2.GetAccountsRequest{
		AdminTransactionId: adminTxID,
	})
	if err != nil {
		return nil, nil, err
	}
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		acct := resp.GetAccount()
		if acct == nil {
			continue
		}
		ids = append(ids, acct.GetId())
		states = append(states, resp.GetAccountState())
	}
	return ids, states, nil
}

// createAndProvisionAccount creates an account and activates it in two
// separate admin transactions (the account must be committed before it can
// be activated).
func createAndProvisionAccount(
	ctx context.Context,
	txClient transactionv1.TransactionServiceClient,
	adminClient adminv2.AdminServiceClient,
) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// --- Transaction 1: create the account ---
	tx1, err := txClient.StartAdminTransaction(ctx, &transactionv1.StartAdminTransactionRequest{})
	if err != nil {
		return "", fmt.Errorf("StartAdminTransaction (create): %w", err)
	}

	createResp, err := adminClient.CreateAccount(ctx, &adminv2.CreateAccountRequest{
		AdminTransactionId: tx1.GetAdminTransactionId(),
		Blob:               []byte("{}"),
	})
	if err != nil {
		return "", fmt.Errorf("CreateAccount: %w", err)
	}
	accountID := createResp.GetAccountId()

	_, err = txClient.CommitAdminTransaction(ctx, &transactionv1.CommitAdminTransactionRequest{
		AdminTransactionId: tx1.GetAdminTransactionId(),
	})
	if err != nil {
		return "", fmt.Errorf("CommitAdminTransaction (create): %w", err)
	}

	// --- Transaction 2: activate the account ---
	tx2, err := txClient.StartAdminTransaction(ctx, &transactionv1.StartAdminTransactionRequest{})
	if err != nil {
		return "", fmt.Errorf("StartAdminTransaction (activate): %w", err)
	}

	_, err = adminClient.ActivateAccount(ctx, &adminv2.ActivateAccountRequest{
		AdminTransactionId: tx2.GetAdminTransactionId(),
		AccountId:          accountID,
	})
	if err != nil {
		return "", fmt.Errorf("ActivateAccount (account=%s): %w", accountID, err)
	}

	_, err = txClient.CommitAdminTransaction(ctx, &transactionv1.CommitAdminTransactionRequest{
		AdminTransactionId: tx2.GetAdminTransactionId(),
	})
	if err != nil {
		return "", fmt.Errorf("CommitAdminTransaction (activate): %w", err)
	}

	return accountID, nil
}
